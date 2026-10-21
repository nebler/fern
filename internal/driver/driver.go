// Package driver runs each environment's box as one agent-sandbox Sandbox.
// Asleep is operatingMode Suspended (no pod); awake is Running.
package driver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
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

const (
	envLabel     = "fern.dev/env"
	partOfLabel  = "app.kubernetes.io/part-of"
	pollInterval = 200 * time.Millisecond
	wakeTimeout  = 2 * time.Minute
	stopTimeout  = time.Minute
	// healthInterval is the pause between asking a waking box's fern-init
	// whether its services are ready. Each question waits up to healthWait
	// for the answer, so fern-init answers the moment they are, and paces
	// its own checks.
	healthInterval = 50 * time.Millisecond
	healthWait     = 5 * time.Second
	// podCheckEvery is how often a pod with an address is looked at again:
	// it may have been replaced or have failed.
	podCheckEvery = time.Second
)

// Config describes the boxes a Driver creates.
type Config struct {
	Namespace string // where Sandboxes live
	Image     string // box image; its entrypoint is fern-init
	Node      string // node boxes are pinned to: the one whose daemon has their disks
	CSIDriver string // name of the CSI driver that mounts live/
	UID, GID  int64  // the box user, who owns live/
}

// Driver manages Sandboxes through the Kubernetes API.
type Driver struct {
	c      client.Client
	cfg    Config
	health *http.Client // asks boxes' fern-init whether they are ready
}

// New returns a Driver that uses c, whose scheme must include corev1 and the
// agent-sandbox types.
func New(c client.Client, cfg Config) *Driver {
	// fern-init answers /healthz within 2.5s. Connecting gets far less: for
	// a moment after a pod starts, the network policy can drop packets to
	// it instead of refusing them, and a wait for the full timeout there
	// would cost seconds.
	health := &http.Client{
		Timeout:   healthWait + 3*time.Second,
		Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext},
	}
	return &Driver{c: c, cfg: cfg, health: health}
}

// Create makes environment env's Sandbox, asleep, with its workspace mounted
// at the path it has on the host. Creating one that exists is not an error.
func (d *Driver) Create(ctx context.Context, env, token, workspace string) error {
	sb := &sandboxv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: env, Namespace: d.cfg.Namespace, Labels: labels(env)},
		Spec: sandboxv1.SandboxSpec{
			OperatingMode:    sandboxv1.SandboxOperatingModeSuspended,
			SandboxBlueprint: sandboxv1.SandboxBlueprint{PodTemplate: d.podTemplate(env, token, workspace)},
		},
	}
	if err := d.c.Create(ctx, sb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create sandbox %s: %w", env, err)
	}
	return nil
}

// Wake starts environment env's box, waits until its services are ready and
// returns the address of its exec server.
//
// Readiness comes from fern-init itself, on the new pod's IP. Waiting for
// kubelet's next probe and the controller's Ready condition would add up to
// a second, and polling them costs Kubernetes API calls.
func (d *Driver) Wake(ctx context.Context, env string) (string, error) {
	if err := d.setMode(ctx, env, sandboxv1.SandboxOperatingModeRunning); err != nil {
		return "", err
	}
	var addr string
	var looked time.Time
	err := pollEvery(ctx, healthInterval, wakeTimeout, func(ctx context.Context) (bool, error) {
		// Look at the pod every pollInterval while it has no address, and
		// every podCheckEvery once it has one.
		if since := time.Since(looked); (addr == "" && since >= pollInterval) || since >= podCheckEvery {
			looked = time.Now()
			var err error
			if addr, err = d.podAddr(ctx, env); err != nil {
				return false, err
			}
		}
		return addr != "" && d.healthy(ctx, addr), nil
	})
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", env, err)
	}
	return addr, nil
}

// podAddr returns the exec address of environment env's running pod, or ""
// if it has none yet. A failed pod is deleted, so the controller makes a new
// one: agent-sandbox doesn't replace a pod that kubelet evicted.
func (d *Driver) podAddr(ctx context.Context, env string) (string, error) {
	pod := &corev1.Pod{}
	err := d.c.Get(ctx, types.NamespacedName{Namespace: d.cfg.Namespace, Name: env}, pod)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get pod %s: %w", env, err)
	}
	if pod.Status.Phase == corev1.PodFailed {
		if err := d.c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("delete failed pod %s: %w", env, err)
		}
		return "", nil
	}
	// An old pod that is still terminating isn't the one being woken.
	if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" {
		return "", nil
	}
	return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(boxexec.Port)), nil
}

// healthy reports whether fern-init at addr says its services are ready.
func (d *Driver) healthy(ctx context.Context, addr string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz?wait="+healthWait.String(), nil)
	if err != nil {
		return false
	}
	resp, err := d.health.Do(req)
	if err != nil {
		return false // not listening yet
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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
	return stateOf(sb), nil
}

// States reports the state of every environment's box, by environment, with
// one Kubernetes call.
func (d *Driver) States(ctx context.Context) (map[string]State, error) {
	var list sandboxv1.SandboxList
	if err := d.c.List(ctx, &list, client.InNamespace(d.cfg.Namespace), client.MatchingLabels{partOfLabel: "fern"}); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	states := make(map[string]State, len(list.Items))
	for i := range list.Items {
		states[list.Items[i].Name] = stateOf(&list.Items[i])
	}
	return states, nil
}

func stateOf(sb *sandboxv1.Sandbox) State {
	if sb.Spec.OperatingMode == sandboxv1.SandboxOperatingModeSuspended {
		// A Sandbox created asleep has no Suspended condition until the
		// controller first sees it; it never had a pod.
		suspended := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1.SandboxConditionSuspended))
		if suspended == nil || suspended.Status == metav1.ConditionTrue {
			return Asleep
		}
		return Sleeping
	}
	if meta.IsStatusConditionTrue(sb.Status.Conditions, string(sandboxv1.SandboxConditionReady)) {
		return Running
	}
	return Waking
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
	return poll(ctx, stopTimeout, func(ctx context.Context) (bool, error) {
		err := d.c.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// poll calls cond every pollInterval until it is done or fails, for at most
// timeout.
func poll(ctx context.Context, timeout time.Duration, cond wait.ConditionWithContextFunc) error {
	return pollEvery(ctx, pollInterval, timeout, cond)
}

// pollEvery calls cond every interval until it is done or fails, for at most
// timeout.
func pollEvery(ctx context.Context, interval, timeout time.Duration, cond wait.ConditionWithContextFunc) error {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := wait.PollUntilContextCancel(pollCtx, interval, true, cond)
	// The client's rate limiter fails early near the deadline, with an
	// error that hides the cause.
	if err != nil && ctx.Err() == nil && pollCtx.Err() != nil {
		return fmt.Errorf("not done after %s: %w", timeout, context.DeadlineExceeded)
	}
	return err
}

func (d *Driver) podTemplate(env, token, workspace string) sandboxv1.PodTemplate {
	uid, gid := d.cfg.UID, d.cfg.GID
	yes, no := true, false
	grace := int64(15)
	tmpSize := resource.MustParse("4Gi")
	return sandboxv1.PodTemplate{
		ObjectMeta: sandboxv1.PodMetadata{Labels: labels(env)},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			NodeSelector:                  map[string]string{corev1.LabelHostname: d.cfg.Node},
			AutomountServiceAccountToken:  &no,
			EnableServiceLinks:            &no,
			TerminationGracePeriodSeconds: &grace,
			// The pause container becomes PID 1 and reaps orphans, which
			// fern-init (busy supervising) doesn't.
			ShareProcessNamespace: &yes,
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
					{Name: "FERN_WORKSPACE", Value: workspace},
				},
				Ports: []corev1.ContainerPort{{Name: "exec", ContainerPort: boxexec.Port}},
				// fern-init answers only when every service passes its ready check.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path: "/healthz", Port: intstr.FromInt32(boxexec.Port),
					}},
					PeriodSeconds: 1,
					// fern-init answers within 2.5s, even when a check hangs.
					TimeoutSeconds: 3,
				},
				// The limits keep one runaway agent from starving the
				// other boxes and the daemon.
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "live", MountPath: workspace},
					{Name: "tmp", MountPath: "/tmp"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "live", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
					Driver:           d.cfg.CSIDriver,
					VolumeAttributes: map[string]string{"env": env},
				}}},
				// HOME is in /tmp, so caches land here; the node's disk is shared.
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpSize}}},
			},
		},
	}
}

func labels(env string) map[string]string {
	return map[string]string{envLabel: env, partOfLabel: "fern"}
}
