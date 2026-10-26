// Package driver runs each environment's box as one agent-sandbox Sandbox.
// The Sandbox and, while the box is awake, its pod are named after the
// environment. Asleep is no Sandbox yet, or one in operatingMode Suspended
// (no pod); awake is Running.
package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/csi"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// State is whether an environment's box is up.
type State string

// The states of a box.
const (
	Asleep   State = "asleep"
	Waking   State = "waking"   // its pod is starting
	Running  State = "running"  // its pod's containers run
	Stopping State = "stopping" // asleep, but its pod isn't gone yet
	Unknown  State = "unknown"  // Kubernetes didn't say
)

const (
	namespace = csi.BoxNamespace
	// claudeSecret's "token" is Claude's login, from `claude setup-token`;
	// lab/claude-login.sh makes it.
	claudeSecret = "claude-login"
	partOfLabel  = "app.kubernetes.io/part-of"

	wakeTimeout = 2 * time.Minute
	goneTimeout = time.Minute
	// readyInterval is the pause between asking a waking box's fern-init
	// whether its services are ready. Each question waits up to readyWait
	// for the answer, so fern-init answers the moment they are, and paces
	// its own checks.
	readyInterval = 50 * time.Millisecond
	readyWait     = 5 * time.Second
	// podInterval is how often a pod is looked at while it has no address,
	// and podCheckEvery once it has one: it may have been replaced or failed.
	podInterval   = 200 * time.Millisecond
	podCheckEvery = time.Second
	// stopGrace bounds a box's stop. fern-init needs at most 8s for the
	// services' stop commands, 3s more to kill them and 1s to finish.
	stopGrace = 15 * time.Second
)

// Driver manages Sandboxes through the Kubernetes API.
type Driver struct {
	c            client.Client
	node         string       // boxes are pinned here: its daemon has their disks
	image        string       // the box image; its entrypoint is fern-init
	runtimeClass string       // the boxes' RuntimeClass, such as gvisor; "" is the node's default
	ready        *http.Client // asks waking boxes' fern-init whether they are ready
	tracer       trace.Tracer
	// steps times the steps of a wake that succeeded: making the Sandbox
	// run, until its pod has an address, and until its services are ready.
	steps *prometheus.HistogramVec
}

// New returns a Driver that runs boxes from image on node under RuntimeClass
// runtimeClass (or the node's default, if ""), through c, whose scheme must
// include corev1 and the agent-sandbox types, and records its steps with tr.
func New(c client.Client, node, image, runtimeClass string, tr trace.Tracer) *Driver {
	// fern-init answers within readyWait and its checks' 2.25s. Connecting
	// gets far less: for a moment after a pod starts, the network policy can
	// drop packets to it instead of refusing them, and a wait for the full
	// timeout there would cost seconds.
	ready := &http.Client{
		Timeout:   readyWait + 3*time.Second,
		Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext},
	}
	steps := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fern_wake_step_seconds",
		Help:    "Time a wake spent in each step: sandbox (the API call), pod (until the pod has an address) and ready (until its services are).",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 15), // 5ms to 82s
	}, []string{"step"})
	return &Driver{c: c, node: node, image: image, runtimeClass: runtimeClass, ready: ready, tracer: tr, steps: steps}
}

// Describe sends the descriptions of the Driver's metrics.
func (d *Driver) Describe(ch chan<- *prometheus.Desc) { d.steps.Describe(ch) }

// Collect sends the Driver's metrics.
func (d *Driver) Collect(ch chan<- prometheus.Metric) { d.steps.Collect(ch) }

// Wake starts environment env's box, waits until its services are ready and
// returns the address of its exec server.
//
// Every wake sets the box up as this daemon would make it, so environments
// made by an older Fern get today's folders and login: agent-sandbox builds
// each new pod from the Sandbox's template and leaves running pods alone.
// Readiness comes from fern-init itself, on the new pod's IP. Waiting for
// kubelet and the controller to report the pod Ready would add up to a
// second, and polling them costs Kubernetes API calls.
func (d *Driver) Wake(ctx context.Context, env store.Env) (string, error) {
	start := time.Now()
	_, span := d.tracer.Start(ctx, "sandbox")
	err := d.apply(ctx, env)
	end(span, err)
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", env.ID, err)
	}
	applied := time.Now()
	_, span = d.tracer.Start(ctx, "pod")
	var addr string
	var looked, found time.Time
	var looks, asks int
	err = poll(ctx, readyInterval, wakeTimeout, func(ctx context.Context) (bool, error) {
		if since := time.Since(looked); (addr == "" && since >= podInterval) || since >= podCheckEvery {
			looked = time.Now()
			looks++
			var err error
			if addr, err = d.podAddr(ctx, env.ID); err != nil {
				return false, err
			}
			if addr != "" && found.IsZero() {
				found = time.Now()
				span.SetAttributes(attribute.Int("fern.pod_looks", looks))
				span.End()
				_, span = d.tracer.Start(ctx, "ready")
			}
		}
		if addr == "" {
			return false, nil
		}
		asks++
		return d.isReady(ctx, addr), nil
	})
	span.SetAttributes(attribute.Int("fern.ready_asks", asks))
	end(span, err)
	if err != nil {
		return "", fmt.Errorf("wake %s: %w", env.ID, err)
	}
	d.steps.WithLabelValues("sandbox").Observe(applied.Sub(start).Seconds())
	d.steps.WithLabelValues("pod").Observe(found.Sub(applied).Seconds())
	d.steps.WithLabelValues("ready").Observe(time.Since(found).Seconds())
	return addr, nil
}

// apply makes environment env's Sandbox, running, or brings the one there up
// to date. A merge patch keeps what others set, such as a runtime class.
func (d *Driver) apply(ctx context.Context, env store.Env) error {
	spec := sandboxv1.SandboxSpec{
		OperatingMode:    sandboxv1.SandboxOperatingModeRunning,
		SandboxBlueprint: sandboxv1.SandboxBlueprint{PodTemplate: d.podTemplate(env)},
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{
		"operatingMode": spec.OperatingMode,
		"podTemplate":   spec.PodTemplate,
	}})
	if err != nil {
		return err
	}
	obj := metav1.ObjectMeta{Name: env.ID, Namespace: namespace, Labels: map[string]string{partOfLabel: "fern"}}
	err = d.c.Patch(ctx, &sandboxv1.Sandbox{ObjectMeta: obj}, client.RawPatch(types.MergePatchType, patch))
	if apierrors.IsNotFound(err) {
		err = d.c.Create(ctx, &sandboxv1.Sandbox{ObjectMeta: obj, Spec: spec})
	}
	return err
}

// podAddr returns the exec address of environment id's running pod, or ""
// if it has none yet. A failed pod is deleted, so the controller makes a new
// one: agent-sandbox doesn't replace a pod that kubelet evicted.
func (d *Driver) podAddr(ctx context.Context, id string) (string, error) {
	pod := &corev1.Pod{}
	err := d.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: id}, pod)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get pod %s: %w", id, err)
	}
	if pod.Status.Phase == corev1.PodFailed {
		if err := d.c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("delete failed pod %s: %w", id, err)
		}
		return "", nil
	}
	// An old pod that is still terminating isn't the one being woken.
	if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" {
		return "", nil
	}
	return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(boxexec.Port)), nil
}

// isReady reports whether fern-init at addr says its services are ready.
func (d *Driver) isReady(ctx context.Context, addr string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+boxexec.ReadyPath+"?wait="+readyWait.String(), nil)
	if err != nil {
		return false
	}
	resp, err := d.ready.Do(req)
	if err != nil {
		return false // not listening yet
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Sleep stops environment id's box and waits until its pod, and with it the
// pod's mounts, is gone. A box with no Sandbox is asleep already.
func (d *Driver) Sleep(ctx context.Context, id string) error {
	patch := fmt.Appendf(nil, `{"spec":{"operatingMode":%q}}`, sandboxv1.SandboxOperatingModeSuspended)
	sb := &sandboxv1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: namespace}}
	_, span := d.tracer.Start(ctx, "suspend")
	err := d.c.Patch(ctx, sb, client.RawPatch(types.MergePatchType, patch))
	if apierrors.IsNotFound(err) {
		span.End()
		return nil
	}
	end(span, err)
	if err != nil {
		return fmt.Errorf("sleep %s: %w", id, err)
	}
	d.stopInit(ctx, id)
	if err := d.waitGone(ctx, id, &corev1.Pod{}); err != nil {
		return fmt.Errorf("sleep %s: %w", id, err)
	}
	return nil
}

// Delete removes environment id's Sandbox and waits until it and its pod are
// gone. Waiting for the Sandbox keeps a quick create of the same ID from
// finding the old one still being deleted.
func (d *Driver) Delete(ctx context.Context, id string) error {
	sb := &sandboxv1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: namespace}}
	_, span := d.tracer.Start(ctx, "delete")
	err := d.c.Delete(ctx, sb, client.PropagationPolicy(metav1.DeletePropagationForeground))
	if apierrors.IsNotFound(err) {
		err = nil
	}
	end(span, err)
	if err != nil {
		return fmt.Errorf("delete sandbox %s: %w", id, err)
	}
	d.stopInit(ctx, id)
	if err := d.waitGone(ctx, id, &sandboxv1.Sandbox{}); err != nil {
		return fmt.Errorf("delete sandbox %s: %w", id, err)
	}
	if err := d.waitGone(ctx, id, &corev1.Pod{}); err != nil {
		return fmt.Errorf("delete sandbox %s: %w", id, err)
	}
	return nil
}

// stopInit lets services shut down while kubelet waits for fresh PLEG status.
// It is only a head start: durable Sandbox suspension/deletion comes first,
// and waitGone still fences the workspace. A crash at either boundary leaves
// Kubernetes responsible for finishing the stop. Older or unreachable boxes
// take the normal kubelet SIGTERM path instead.
func (d *Driver) stopInit(ctx context.Context, id string) {
	ctx, span := d.tracer.Start(ctx, "stop_init")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	var pod corev1.Pod
	if err := d.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: id}, &pod); err != nil {
		return
	}
	if pod.Status.PodIP == "" || pod.UID == "" {
		return
	}
	var token string
	for _, c := range pod.Spec.Containers {
		if c.Name != "box" {
			continue
		}
		for _, v := range c.Env {
			if v.Name == "FERN_BOX_TOKEN" {
				token = v.Value
			}
		}
	}
	if token == "" {
		return
	}
	addr := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(boxexec.Port))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+boxexec.StopPath, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-Match", strconv.Quote(string(pod.UID)))
	resp, err := d.ready.Do(req)
	if err != nil {
		return
	}
	if err := resp.Body.Close(); err != nil {
		return
	}
}

// States reports the state of every box that has a Sandbox, by environment
// ID, with one Kubernetes call. An environment that has none is asleep.
func (d *Driver) States(ctx context.Context) (map[string]State, error) {
	var list sandboxv1.SandboxList
	if err := d.c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{partOfLabel: "fern"}); err != nil {
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
		// A Sandbox the controller hasn't seen since it was suspended has no
		// Suspended condition yet.
		suspended := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1.SandboxConditionSuspended))
		if suspended == nil || suspended.Status == metav1.ConditionTrue {
			return Asleep
		}
		return Stopping
	}
	if meta.IsStatusConditionTrue(sb.Status.Conditions, string(sandboxv1.SandboxConditionReady)) {
		return Running
	}
	return Waking
}

// waitGone waits until the object named id of obj's kind doesn't exist.
func (d *Driver) waitGone(ctx context.Context, id string, obj client.Object) error {
	key := types.NamespacedName{Namespace: namespace, Name: id}
	_, span := d.tracer.Start(ctx, "wait_gone", trace.WithAttributes(attribute.String("fern.kind", fmt.Sprintf("%T", obj))))
	looks := 0
	err := poll(ctx, podInterval, goneTimeout, func(ctx context.Context) (bool, error) {
		looks++
		err := d.c.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	span.SetAttributes(attribute.Int("fern.looks", looks))
	end(span, err)
	return err
}

// end ends span, marking it failed if err isn't nil.
func end(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// poll calls cond every interval until it is done or fails, for at most
// timeout.
func poll(ctx context.Context, interval, timeout time.Duration, cond wait.ConditionWithContextFunc) error {
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

// podTemplate returns the pod env's box runs as: fern-init, with the
// environment's folders at their own paths, Claude's login and the box's
// limits.
func (d *Driver) podTemplate(env store.Env) sandboxv1.PodTemplate {
	vars := []corev1.EnvVar{
		{Name: "FERN_BOX_TOKEN", Value: env.BoxToken},
		{Name: "FERN_POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "FERN_WORKSPACE", Value: env.Workspace},
	}
	// A golden builder's box runs no agent and gets no login: what its
	// services write goes into every thread of the repository.
	if setup.IsBuilder(env.ID) {
		env.Claude = ""
	} else {
		vars = append(vars,
			// Claude keeps its transcripts here, so `--resume` works after a
			// sleep or a restore, which both replace the box.
			corev1.EnvVar{Name: "CLAUDE_CONFIG_DIR", Value: env.Claude},
			// The login comes from the Secret when the pod starts, so it is
			// never in the Sandbox, the index or a file. Boxes start without
			// it until it exists.
			corev1.EnvVar{Name: "CLAUDE_CODE_OAUTH_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: claudeSecret},
				Key:                  "token",
				Optional:             new(true),
			}}})
	}
	// HOME is /tmp (images/Dockerfile), so caches land here; capped
	// because every box shares the node's disk.
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	volumes := []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("4Gi"))},
	}}}
	for _, f := range []struct{ name, path string }{
		{csi.FolderWorkspace, env.Workspace},
		{csi.FolderClaude, env.Claude},
		{csi.FolderGit, env.Git},
	} {
		if f.path == "" {
			continue // the workspace isn't a worktree of a repository under repos/
		}
		// The git folder is read-only, or a box could change the repository's
		// config or hooks and so run code in T3's server, which runs git
		// there. The container runtime enforces it.
		mounts = append(mounts, corev1.VolumeMount{Name: f.name, MountPath: f.path, ReadOnly: f.name == csi.FolderGit})
		volumes = append(volumes, corev1.Volume{Name: f.name, VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
			Driver:           csi.Name,
			VolumeAttributes: map[string]string{csi.AttrEnv: env.ID, csi.AttrFolder: f.name},
		}}})
	}
	if env.Git != "" {
		// The worktree's .git file names its git folder, and T3's server
		// runs git by it: a box that pointed it at a folder of its own could
		// run code in T3 through that folder's config. Read-only, it keeps
		// naming the read-only git folder.
		mounts = append(mounts, corev1.VolumeMount{
			Name: csi.FolderWorkspace, MountPath: filepath.Join(env.Workspace, ".git"), SubPath: ".git", ReadOnly: true,
		})
	}
	var class *string // unset, so a wake keeps a runtime class set by hand
	if d.runtimeClass != "" {
		class = &d.runtimeClass
	}
	return sandboxv1.PodTemplate{Spec: corev1.PodSpec{
		NodeSelector:                  map[string]string{corev1.LabelHostname: d.node},
		RuntimeClassName:              class,
		AutomountServiceAccountToken:  new(false),
		EnableServiceLinks:            new(false),
		TerminationGracePeriodSeconds: new(int64(stopGrace / time.Second)),
		// The pause container becomes PID 1 and reaps orphans, which
		// fern-init (busy supervising) doesn't.
		ShareProcessNamespace: new(true),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser: new(int64(store.BoxUID)), RunAsGroup: new(int64(store.BoxGID)), RunAsNonRoot: new(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:  "box",
			Image: d.image,
			Env:   vars,
			// The limits keep one runaway agent from starving the other boxes
			// and the daemon.
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
				AllowPrivilegeEscalation: new(false),
				ReadOnlyRootFilesystem:   new(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			VolumeMounts: mounts,
		}},
		Volumes: volumes,
	}}
}
