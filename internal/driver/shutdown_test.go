package driver_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/driver"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// stopClient interrupts a stop at the boundary after its durable mutation.
type stopClient struct {
	client.Client
	cancel context.CancelFunc
	fail   error
}

func (c stopClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.fail != nil {
		return c.fail
	}
	if err := c.Client.Patch(ctx, obj, patch, opts...); err != nil {
		return err
	}
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

func (c stopClient) Get(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c stopClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.fail != nil {
		return c.fail
	}
	if err := c.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

func stopObjects() (*sandboxv1.Sandbox, *corev1.Pod) {
	meta := metav1.ObjectMeta{Name: "demo", Namespace: "fern-boxes"}
	podMeta := meta
	podMeta.UID = "pod-1"
	return &sandboxv1.Sandbox{ObjectMeta: meta, Spec: sandboxv1.SandboxSpec{OperatingMode: sandboxv1.SandboxOperatingModeRunning}},
		&corev1.Pod{ObjectMeta: podMeta, Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "box", Env: []corev1.EnvVar{{Name: "FERN_BOX_TOKEN", Value: "tok"}},
		}}}, Status: corev1.PodStatus{PodIP: "127.0.0.1"}}
}

func serveStop(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	// The public driver uses the protocol's fixed port, just as a real pod does.
	lc := net.ListenConfig{}
	ln, err := lc.Listen(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(boxexec.Port)))
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
}

func TestStopShutdownReplay(t *testing.T) {
	for _, op := range []struct {
		name string
		run  func(*driver.Driver, context.Context, string) error
	}{
		{name: "sleep", run: (*driver.Driver).Sleep},
		{name: "delete", run: (*driver.Driver).Delete},
	} {
		t.Run(op.name, func(t *testing.T) {
			for _, boundary := range []string{"intent recorded", "shutdown requested"} {
				t.Run(boundary, func(t *testing.T) {
					sb, pod := stopObjects()
					_, c := newDriver(t, sb, pod)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var calls atomic.Int32
					serveStop(t, func(w http.ResponseWriter, r *http.Request) {
						n := calls.Add(1)
						if r.Method != http.MethodPost || r.URL.Path != boxexec.StopPath || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("If-Match") != `"pod-1"` {
							t.Error("shutdown request has wrong method, path, authorization or pod UID")
						}
						var current sandboxv1.Sandbox
						err := c.Get(r.Context(), client.ObjectKeyFromObject(sb), &current)
						switch {
						case op.name == "delete":
							if !apierrors.IsNotFound(err) {
								t.Errorf("Sandbox at shutdown error = %v, want not found", err)
							}
						case err != nil:
							t.Errorf("get durable suspension at shutdown: %v", err)
						case current.Spec.OperatingMode != sandboxv1.SandboxOperatingModeSuspended:
							t.Errorf("operating mode at shutdown = %s, want Suspended", current.Spec.OperatingMode)
						}
						if boundary == "shutdown requested" && n == 1 {
							cancel()
						} else if err := c.Delete(r.Context(), pod); err != nil {
							t.Errorf("finish Kubernetes cleanup: %v", err)
						}
						w.WriteHeader(http.StatusNoContent)
					})
					interrupted := stopClient{Client: c}
					if boundary == "intent recorded" {
						interrupted.cancel = cancel
					}
					d := driver.New(interrupted, "node-1", "fern-box:test", "", notrace)
					if err := op.run(d, ctx, "demo"); !errors.Is(err, context.Canceled) {
						t.Fatalf("interrupted stop error = %v, want canceled", err)
					}
					if op.name == "sleep" {
						if got := sandbox(t, c, "demo").Spec.OperatingMode; got != sandboxv1.SandboxOperatingModeSuspended {
							t.Errorf("operating mode after interruption = %s, want Suspended", got)
						}
					} else if err := c.Get(t.Context(), client.ObjectKeyFromObject(sb), &sandboxv1.Sandbox{}); !apierrors.IsNotFound(err) {
						t.Errorf("Sandbox after interruption error = %v, want not found", err)
					}
					if err := c.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
						t.Fatalf("pod before replay: %v", err)
					}
					d = driver.New(c, "node-1", "fern-box:test", "", notrace)
					if err := op.run(d, t.Context(), "demo"); err != nil {
						t.Fatalf("replayed stop: %v", err)
					}
					if err := c.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
						t.Errorf("pod after replay error = %v, want not found", err)
					}
					if err := op.run(d, t.Context(), "demo"); err != nil {
						t.Errorf("repeated stop: %v", err)
					}
				})
			}
		})
	}
}

func TestSleepDoesNotStopBeforeSuspension(t *testing.T) {
	sb, pod := stopObjects()
	_, c := newDriver(t, sb, pod)
	var calls atomic.Int32
	serveStop(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	want := errors.New("suspension refused")
	d := driver.New(stopClient{Client: c, fail: want}, "node-1", "fern-box:test", "", notrace)
	if err := d.Sleep(t.Context(), "demo"); !errors.Is(err, want) {
		t.Errorf("Sleep error = %v, want suspension refusal", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("shutdown requests = %d, want 0", got)
	}
}

func TestSleepWithOlderInit(t *testing.T) {
	sb, pod := stopObjects()
	d, c := newDriver(t, sb, pod)
	serveStop(t, func(w http.ResponseWriter, r *http.Request) {
		// Kubelet still removes the pod when fern-init has no stop endpoint.
		if err := c.Delete(r.Context(), pod); err != nil {
			t.Errorf("Kubernetes cleanup: %v", err)
		}
		http.NotFound(w, r)
	})
	if err := d.Sleep(t.Context(), "demo"); err != nil {
		t.Errorf("Sleep with old init: %v", err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("pod after sleep error = %v, want not found", err)
	}
}
