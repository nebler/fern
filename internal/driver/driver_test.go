package driver_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

func newDriver(t *testing.T, objs ...client.Object) (*driver.Driver, client.Client) {
	t.Helper()
	return newDriverWithClass(t, "", objs...)
}

func newDriverWithClass(t *testing.T, class string, objs ...client.Object) (*driver.Driver, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sandboxv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return driver.New(c, "node-1", "fern-box:test", class), c
}

// wake runs Wake until it has set the Sandbox up; with no pod to wait for, it
// then times out.
func wake(t *testing.T, d *driver.Driver, env store.Env) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := d.Wake(ctx, env); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wake(%s) error = %v, want it to time out waiting for a pod", env.ID, err)
	}
}

func sandbox(t *testing.T, c client.Client, id string) *sandboxv1.Sandbox {
	t.Helper()
	sb := &sandboxv1.Sandbox{}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "fern-boxes", Name: id}, sb); err != nil {
		t.Fatalf("get sandbox %s: %v", id, err)
	}
	return sb
}

// mounts returns the box container's mounts as path: read-only.
func mounts(sb *sandboxv1.Sandbox) map[string]bool {
	m := make(map[string]bool)
	for _, vm := range sb.Spec.PodTemplate.Spec.Containers[0].VolumeMounts {
		m[vm.MountPath] = vm.ReadOnly
	}
	return m
}

var env = store.Env{
	ID: "demo", Workspace: "/pool/worktrees/r/demo", Claude: "/pool/envs/demo/claude",
	Git: "/pool/repos/r/.git", BoxToken: "tok",
}

func TestWakeSetsUpTheBox(t *testing.T) {
	d, c := newDriver(t)
	wake(t, d, env)
	sb := sandbox(t, c, "demo")
	if sb.Spec.OperatingMode != sandboxv1.SandboxOperatingModeRunning {
		t.Errorf("operating mode %s, want Running", sb.Spec.OperatingMode)
	}
	// The repository's git folder and the worktree's .git file, which names
	// it, must be read-only: a box that could change the folder's config or
	// hooks, or point the file elsewhere, could run code in T3's server.
	want := map[string]bool{
		"/tmp": false, env.Workspace: false, env.Claude: false, env.Git: true, env.Workspace + "/.git": true,
	}
	if diff := cmp.Diff(want, mounts(sb)); diff != "" {
		t.Errorf("mounts (path: read-only) mismatch (-want +got):\n%s", diff)
	}
	vars := make(map[string]corev1.EnvVar)
	for _, v := range sb.Spec.PodTemplate.Spec.Containers[0].Env {
		vars[v.Name] = v
	}
	if ref := vars["FERN_POD_UID"].ValueFrom; ref == nil || ref.FieldRef == nil || ref.FieldRef.FieldPath != "metadata.uid" {
		t.Errorf("FERN_POD_UID = %+v, want the pod's UID from the downward API", ref)
	}
	if got := vars["CLAUDE_CONFIG_DIR"].Value; got != env.Claude {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", got, env.Claude)
	}
	// The login comes from the Secret when the pod starts: never a value
	// written into the Sandbox.
	login := vars["CLAUDE_CODE_OAUTH_TOKEN"]
	if login.Value != "" || login.ValueFrom == nil || login.ValueFrom.SecretKeyRef == nil {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN = %+v, want it from a Secret", login)
	}
}

func TestWakeWithoutGitFolder(t *testing.T) {
	d, c := newDriver(t)
	noGit := env
	noGit.Git = ""
	wake(t, d, noGit)
	want := map[string]bool{"/tmp": false, env.Workspace: false, env.Claude: false}
	if diff := cmp.Diff(want, mounts(sandbox(t, c, "demo"))); diff != "" {
		t.Errorf("a box whose workspace isn't a worktree: mounts mismatch (-want +got):\n%s", diff)
	}
}

// A box made by an older Fern gets today's setup at its next wake, and keeps
// what others set on it.
func TestWakeUpdatesAnOldBox(t *testing.T) {
	old := &sandboxv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "fern-boxes"},
		Spec: sandboxv1.SandboxSpec{
			OperatingMode: sandboxv1.SandboxOperatingModeSuspended,
			SandboxBlueprint: sandboxv1.SandboxBlueprint{PodTemplate: sandboxv1.PodTemplate{Spec: corev1.PodSpec{
				RuntimeClassName: new("gvisor"),
				Containers:       []corev1.Container{{Name: "box", Image: "fern-box:old"}},
			}}},
		},
	}
	d, c := newDriver(t, old)
	wake(t, d, env)
	sb := sandbox(t, c, "demo")
	if _, ok := mounts(sb)[env.Claude]; !ok || sb.Spec.PodTemplate.Spec.Containers[0].Image != "fern-box:test" {
		t.Errorf("after wake: image %s, mounts %v; want today's image and Claude's folder",
			sb.Spec.PodTemplate.Spec.Containers[0].Image, mounts(sb))
	}
	if rc := sb.Spec.PodTemplate.Spec.RuntimeClassName; rc == nil || *rc != "gvisor" {
		t.Errorf("after wake: runtime class %v, want gvisor kept", rc)
	}
}

func TestWakeRuntimeClass(t *testing.T) {
	kata := &sandboxv1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "fern-boxes"},
		Spec: sandboxv1.SandboxSpec{SandboxBlueprint: sandboxv1.SandboxBlueprint{PodTemplate: sandboxv1.PodTemplate{Spec: corev1.PodSpec{
			RuntimeClassName: new("kata"),
			Containers:       []corev1.Container{{Name: "box", Image: "fern-box:old"}},
		}}}},
	}
	for _, tt := range []struct {
		name, class, env string
		want             *string
	}{
		{name: "new box", class: "gvisor", env: "demo", want: new("gvisor")},
		{name: "new box, node's default", class: "", env: "demo", want: nil},
		{name: "box whose class was set by hand", class: "gvisor", env: "set", want: new("gvisor")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, c := newDriverWithClass(t, tt.class, kata.DeepCopy())
			e := env
			e.ID = tt.env
			wake(t, d, e)
			got := sandbox(t, c, tt.env).Spec.PodTemplate.Spec.RuntimeClassName
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("runtime class mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStates(t *testing.T) {
	box := func(id string, mode sandboxv1.SandboxOperatingMode, conds ...metav1.Condition) *sandboxv1.Sandbox {
		return &sandboxv1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: "fern-boxes", Labels: map[string]string{"app.kubernetes.io/part-of": "fern"}},
			Spec:       sandboxv1.SandboxSpec{OperatingMode: mode},
			Status:     sandboxv1.SandboxStatus{Conditions: conds},
		}
	}
	cond := func(typ sandboxv1.ConditionType, status metav1.ConditionStatus) metav1.Condition {
		return metav1.Condition{Type: string(typ), Status: status, Reason: "Test"}
	}
	d, _ := newDriver(t,
		box("new", sandboxv1.SandboxOperatingModeSuspended),
		box("asleep", sandboxv1.SandboxOperatingModeSuspended, cond(sandboxv1.SandboxConditionSuspended, metav1.ConditionTrue)),
		box("stopping", sandboxv1.SandboxOperatingModeSuspended, cond(sandboxv1.SandboxConditionSuspended, metav1.ConditionFalse)),
		box("waking", sandboxv1.SandboxOperatingModeRunning),
		box("running", sandboxv1.SandboxOperatingModeRunning, cond(sandboxv1.SandboxConditionReady, metav1.ConditionTrue)),
	)
	got, err := d.States(t.Context())
	if err != nil {
		t.Fatalf("States() error: %v", err)
	}
	want := map[string]driver.State{
		"new": driver.Asleep, "asleep": driver.Asleep, "stopping": driver.Stopping,
		"waking": driver.Waking, "running": driver.Running,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("States() mismatch (-want +got):\n%s", diff)
	}
}

// An environment that was never woken has no Sandbox: it is asleep, and
// there is nothing to delete.
func TestSleepAndDeleteWithoutSandbox(t *testing.T) {
	d, _ := newDriver(t)
	if err := d.Sleep(t.Context(), "demo"); err != nil {
		t.Errorf("Sleep(demo) error: %v", err)
	}
	if err := d.Delete(t.Context(), "demo"); err != nil {
		t.Errorf("Delete(demo) error: %v", err)
	}
}

// A golden builder's box gets no Claude login and no Claude folder.
func TestWakeBuilderHasNoCredentials(t *testing.T) {
	d, c := newDriver(t)
	builder := env
	builder.ID = setup.ID("sample")
	wake(t, d, builder)
	spec := sandbox(t, c, builder.ID).Spec.PodTemplate.Spec
	for _, v := range spec.Containers[0].Env {
		if v.Name == "CLAUDE_CODE_OAUTH_TOKEN" || v.Name == "CLAUDE_CONFIG_DIR" {
			t.Errorf("builder's box has %s, want no Claude login or folder", v.Name)
		}
	}
	if _, ok := mounts(sandbox(t, c, builder.ID))[env.Claude]; ok {
		t.Errorf("builder's box mounts Claude's folder %s", env.Claude)
	}
}
