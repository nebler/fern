// Package driver runs each environment's box as one agent-sandbox Sandbox.
// Asleep is operatingMode Suspended (no pod); awake is Running.
package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/store"
)

// State is whether an environment's box is up.
type State string

// The states of a box.
const (
	Asleep   State = "asleep"
	Waking   State = "waking"
	Running  State = "running"
	Sleeping State = "sleeping"
)

// ErrNotFound means the environment has no Sandbox.
var ErrNotFound = errors.New("no sandbox")

// Workspace is where an environment's live/ is mounted in its box.
const Workspace = "/workspace"

const (
	envLabel     = "fern.dev/env"
	partOfLabel  = "app.kubernetes.io/part-of"
	pollInterval = 200 * time.Millisecond
	wakeTimeout  = 2 * time.Minute
	stopTimeout  = time.Minute
)

// Config describes the boxes a Driver creates.
type Config struct {
	Namespace string // where Sandboxes live
	Image     string // box image; its entrypoint is fern-init
	Node      string // node boxes are pinned to: the one whose daemon has their disks
	CSIDriver string // name of the CSI driver that mounts live/
}

// Driver manages Sandboxes through the Kubernetes API.
type Driver struct {
	c   client.Client
	cfg Config
}

// New returns a Driver that uses c, whose scheme must include corev1 and the
// agent-sandbox types.
func New(c client.Client, cfg Config) *Driver {
	return &Driver{c: c, cfg: cfg}
}

// Create makes environment env's Sandbox, asleep. Creating one that exists
// is not an error.
func (d *Driver) Create(ctx context.Context, env, token string) error {
	sb := &sandboxv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: env, Namespace: d.cfg.Namespace, Labels: labels(env)},
		Spec: sandboxv1.SandboxSpec{
			OperatingMode:    sandboxv1.SandboxOperatingModeSuspended,
			SandboxBlueprint: sandboxv1.SandboxBlueprint{PodTemplate: d.podTemplate(env, token)},
		},
	}
	if err := d.c.Create(ctx, sb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create sandbox %s: %w", env, err)
	}
	return nil
}

// Wake starts environment env's box, waits until it is ready and returns the
// address of its exec server.
func (d *Driver) Wake(ctx context.Context, env string) (string, error) {
	if err := d.setMode(ctx, env, sandboxv1.SandboxOperatingModeRunning); err != nil {
		return "", err
	}
	var addr string
	err := wait.PollUntilContextTimeout(ctx, pollInterval, wakeTimeout, true, func(ctx context.Context) (bool, error) {
		sb, err := d.get(ctx, env)
		if err != nil {
			return false, err
		}
		ready := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1.SandboxConditionReady))
		if ready == nil || ready.Status != metav1.ConditionTrue ||
			ready.ObservedGeneration < sb.Generation || len(sb.Status.PodIPs) == 0 {
			return false, nil
		}
		addr = net.JoinHostPort(sb.Status.PodIPs[0], strconv.Itoa(boxexec.Port))
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", env, err)
	}
	return addr, nil
}

// Sleep stops environment env's box and waits until its pod, and with it the
// pod's mount of live/, is gone.
func (d *Driver) Sleep(ctx context.Context, env string) error {
	if err := d.setMode(ctx, env, sandboxv1.SandboxOperatingModeSuspended); err != nil {
		return err
	}
	if err := d.waitGone(ctx, env, &corev1.Pod{}); err != nil {
		return fmt.Errorf("sleep %s: %w", env, err)
	}
	return nil
}

// Delete removes environment env's Sandbox and waits until its pod is gone.
func (d *Driver) Delete(ctx context.Context, env string) error {
	sb := &sandboxv1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: env, Namespace: d.cfg.Namespace}}
	err := d.c.Delete(ctx, sb, client.PropagationPolicy(metav1.DeletePropagationForeground))
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete sandbox %s: %w", env, err)
	}
	if err := d.waitGone(ctx, env, &sandboxv1.Sandbox{}); err != nil {
		return fmt.Errorf("delete sandbox %s: %w", env, err)
	}
	if err := d.waitGone(ctx, env, &corev1.Pod{}); err != nil {
		return fmt.Errorf("delete sandbox %s: %w", env, err)
	}
	return nil
}

// State reports whether environment env's box is up.
func (d *Driver) State(ctx context.Context, env string) (State, error) {
	sb, err := d.get(ctx, env)
	if err != nil {
		return "", err
	}
	if sb.Spec.OperatingMode == sandboxv1.SandboxOperatingModeSuspended {
		// A Sandbox created asleep has no Suspended condition until the
		// controller first sees it; it never had a pod.
		suspended := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1.SandboxConditionSuspended))
		if suspended == nil || suspended.Status == metav1.ConditionTrue {
			return Asleep, nil
		}
		return Sleeping, nil
	}
	if meta.IsStatusConditionTrue(sb.Status.Conditions, string(sandboxv1.SandboxConditionReady)) {
		return Running, nil
	}
	return Waking, nil
}

func (d *Driver) get(ctx context.Context, env string) (*sandboxv1.Sandbox, error) {
	sb := &sandboxv1.Sandbox{}
	err := d.c.Get(ctx, types.NamespacedName{Namespace: d.cfg.Namespace, Name: env}, sb)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("environment %s: %w", env, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get sandbox %s: %w", env, err)
	}
	return sb, nil
}

func (d *Driver) setMode(ctx context.Context, env string, mode sandboxv1.SandboxOperatingMode) error {
	sb := &sandboxv1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: env, Namespace: d.cfg.Namespace}}
	patch := fmt.Appendf(nil, `{"spec":{"operatingMode":%q}}`, mode)
	err := d.c.Patch(ctx, sb, client.RawPatch(types.MergePatchType, patch))
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("environment %s: %w", env, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("set %s to %s: %w", env, mode, err)
	}
	return nil
}

// waitGone waits until the object named env of obj's kind doesn't exist. The
// box pod has the Sandbox's name.
func (d *Driver) waitGone(ctx context.Context, env string, obj client.Object) error {
	key := types.NamespacedName{Namespace: d.cfg.Namespace, Name: env}
	return wait.PollUntilContextTimeout(ctx, pollInterval, stopTimeout, true, func(ctx context.Context) (bool, error) {
		err := d.c.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

func (d *Driver) podTemplate(env, token string) sandboxv1.PodTemplate {
	uid, gid := int64(store.BoxUID), int64(store.BoxGID)
	yes, no := true, false
	grace := int64(15)
	return sandboxv1.PodTemplate{
		ObjectMeta: sandboxv1.PodMetadata{Labels: labels(env)},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			NodeSelector:                  map[string]string{corev1.LabelHostname: d.cfg.Node},
			AutomountServiceAccountToken:  &no,
			EnableServiceLinks:            &no,
			TerminationGracePeriodSeconds: &grace,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &uid, RunAsGroup: &gid, RunAsNonRoot: &yes,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "box",
				Image:           d.cfg.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env: []corev1.EnvVar{
					{Name: "FERN_ENV", Value: env},
					{Name: "FERN_TOKEN", Value: token},
					{Name: "FERN_WORKSPACE", Value: Workspace},
				},
				Ports: []corev1.ContainerPort{{Name: "exec", ContainerPort: boxexec.Port}},
				// fern-init answers only when every service passes its ready check.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/healthz", Port: intstr.FromInt32(boxexec.Port),
					}},
					PeriodSeconds: 1,
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "live", MountPath: Workspace},
					{Name: "tmp", MountPath: "/tmp"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "live", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
					Driver:           d.cfg.CSIDriver,
					VolumeAttributes: map[string]string{"env": env},
				}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
}

func labels(env string) map[string]string {
	return map[string]string{envLabel: env, partOfLabel: "fern"}
}
