// Command controller is the always-running pod that accepts work requests
// over HTTP and runs each one in a freshly spawned, short-lived worker pod.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/techiek77-ui/backup/internal/api"
	"github.com/techiek77-ui/backup/internal/config"
	"github.com/techiek77-ui/backup/internal/spawner"
	"github.com/techiek77-ui/backup/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	client, mode, err := newClient(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	log.Info("starting podspawner controller",
		"namespace", cfg.Namespace,
		"nodeSelector", cfg.NodeSelector,
		"tolerations", len(cfg.Tolerations),
		"imagePullSecrets", cfg.ImagePullSecrets,
		"maxConcurrent", cfg.MaxConcurrent,
		"defaultTimeout", cfg.DefaultTimeout,
		"scheduleTimeout", cfg.ScheduleTimeout,
		"imagePullTimeout", cfg.ImagePullTimeout,
		"apiRetryWindow", cfg.APIRetryWindow,
		"sweepInterval", cfg.SweepInterval,
		"clientMode", mode,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st := store.New()
	sp := spawner.New(client, spawner.Config{
		Namespace:        cfg.Namespace,
		NodeSelector:     cfg.NodeSelector,
		Tolerations:      cfg.Tolerations,
		ImagePullSecrets: cfg.ImagePullSecrets,
		ServiceAccount:   cfg.WorkerSA,
		DefaultTimeout:   cfg.DefaultTimeout,
		ScheduleTimeout:  cfg.ScheduleTimeout,
		ImagePullTimeout: cfg.ImagePullTimeout,
		APIRetryWindow:   cfg.APIRetryWindow,
		MaxLogBytes:      cfg.MaxLogBytes,
		KeepFailedPods:   cfg.KeepFailedPods,
	}, st, log)

	// Jobs are tracked in memory only, so anything left from a previous
	// instance can never be collected. Reap it now, then keep sweeping for
	// pods whose delete failed (e.g. during an API-server outage).
	if n, err := sp.Sweep(ctx); err != nil {
		return fmt.Errorf("startup sweep: %w", err)
	} else if n > 0 {
		log.Warn("deleted orphaned worker pods from a previous run", "count", n)
	}
	go sp.RunSweeper(ctx, cfg.SweepInterval)

	srv := api.New(ctx, st, sp, api.Options{
		MaxConcurrent: cfg.MaxConcurrent,
		DefaultImage:  cfg.DefaultImage,
	}, log)

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down: in-flight jobs will be aborted and their pods deleted")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	if err := srv.Wait(shutdownCtx); err != nil {
		log.Warn("some jobs did not finish cleaning up before deadline", "err", err)
	}
	return nil
}

// newClient prefers in-cluster config and falls back to a kubeconfig so the
// controller can also be run from a laptop against kind / OKE for debugging.
func newClient(kubeconfig string) (kubernetes.Interface, string, error) {
	if rc, err := rest.InClusterConfig(); err == nil {
		c, err := kubernetes.NewForConfig(rc)
		return c, "in-cluster", err
	}
	if kubeconfig == "" {
		if home, err := os.UserHomeDir(); err == nil {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	rc, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, "", fmt.Errorf("not in-cluster and kubeconfig %q unusable: %w", kubeconfig, err)
	}
	c, err := kubernetes.NewForConfig(rc)
	return c, "kubeconfig", err
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
