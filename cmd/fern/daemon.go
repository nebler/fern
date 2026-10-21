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

// runDaemon serves the API and the CSI plugin until ctx is cancelled.
func runDaemon(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pool := flags.String("pool", "/var/lib/fern", "btrfs pool mount point")
	socket := flags.String("socket", defaultSocket, "API socket")
	csiSocket := flags.String("csi-socket", "/csi/csi.sock", "CSI plugin socket")
	node := flags.String("node", os.Getenv("NODE_NAME"), "this node's name")
	namespace := flags.String("namespace", "fern-boxes", "namespace of box Sandboxes")
	image := flags.String("box-image", "fern-box:dev", "box image")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *node == "" {
		return errors.New("--node or NODE_NAME is required")
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))

	st, err := store.Open(ctx, *pool)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	kube, err := kubeClient()
	if err != nil {
		return err
	}
	drv := driver.New(kube, driver.Config{
		Namespace: *namespace, Image: *image, Node: *node, CSIDriver: csi.Name,
		UID: store.BoxUID, GID: store.BoxGID,
	})

	apiLis, err := listenUnix(ctx, *socket)
	if err != nil {
		return err
	}
	csiLis, err := listenUnix(ctx, *csiSocket)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Handler:           api.NewServer(st, drv, log).Handler(),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
	}
	grpcSrv := grpc.NewServer()
	csi.New(*node, st.LivePath, log).Register(grpcSrv)

	log.InfoContext(ctx, "fern daemon started",
		slog.String("node", *node), slog.String("pool", *pool), slog.String("socket", *socket))
	g, ctx := errgroup.WithContext(ctx)
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
	return g.Wait()
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
// lets only the owner (root) connect.
func listenUnix(ctx context.Context, path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return nil, err
	}
	return lis, nil
}
