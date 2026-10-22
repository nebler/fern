package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	sandboxv1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/csi"
	"github.com/nebler/fern/internal/driver"
	"github.com/nebler/fern/internal/store"
)

// Where the daemon finds the pool and serves the CSI plugin; deploy/fern.yaml
// mounts both.
const (
	pool      = "/var/lib/fern"
	csiSocket = "/csi/csi.sock"
)

// runDaemon serves the API and the CSI plugin until ctx is cancelled, and
// returns the process exit code.
func runDaemon(ctx context.Context, args []string, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if err := serve(ctx, args, log); err != nil {
		log.ErrorContext(ctx, "fern daemon failed", slog.Any("error", err))
		return 1
	}
	return 0
}

func serve(ctx context.Context, args []string, log *slog.Logger) error {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	image := flags.String("box-image", "fern-box:dev", "box image")
	if err := flags.Parse(args); err != nil {
		return err
	}
	node := os.Getenv("NODE_NAME")
	if node == "" {
		return errors.New("NODE_NAME is not set")
	}

	st, err := store.Open(ctx, pool)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	// Finish what a crash interrupted before anyone can see the half-done state.
	lost, err := st.Reconcile(ctx)
	if err != nil {
		return err
	}
	for _, dir := range lost {
		log.WarnContext(ctx, "environment directory not in the index; left for a person to remove", slog.String("dir", dir))
	}
	kube, err := kubeClient()
	if err != nil {
		return err
	}
	// T3's server runs as the box user, and uses the API too.
	apiLis, err := listenUnix(ctx, socket, store.BoxGID)
	if err != nil {
		return err
	}
	csiLis, err := listenUnix(ctx, csiSocket, -1)
	if err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	apiSrv := api.NewServer(st, driver.New(kube, node, *image), log)
	httpSrv := &http.Server{
		Handler:           apiSrv.Handler(),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
	}
	grpcSrv := grpc.NewServer()
	csi.New(node, st.Get, log).Register(grpcSrv)
	log.InfoContext(ctx, "fern daemon started", slog.String("node", node))
	g.Go(func() error {
		if err := httpSrv.Serve(apiLis); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("api: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := grpcSrv.Serve(csiLis); err != nil {
			return fmt.Errorf("csi: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		grpcSrv.GracefulStop()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	})
	err = g.Wait()
	// Changes outlive their requests; the store must stay open until they end.
	apiSrv.Wait()
	return err
}

func kubeClient() (client.Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	// client-go's default of 5 requests a second made waking 10 boxes at
	// once take 6.5s: every wake polls its Sandbox and pod.
	cfg.QPS, cfg.Burst = 50, 100
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := sandboxv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return c, nil
}

// listenUnix listens on a unix socket at path, replacing a stale one, and
// lets only the owner (root) connect, and group gid if it isn't -1.
func listenUnix(ctx context.Context, path string, gid int) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0o600)
	if gid != -1 {
		mode = 0o660
		if err := os.Chown(path, -1, gid); err != nil {
			_ = lis.Close()
			return nil, err
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = lis.Close()
		return nil, err
	}
	return lis, nil
}
