// Command gw is the infrastructure-facing Gateway. On startup it launches every
// configured target via the Runtime and begins readiness polling; it serves the
// target list and per-target attach WebSocket; on SIGINT/SIGTERM it stops the
// whole fleet.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abyss0-dev/traced-web-terminal/internal/gw"
	"github.com/abyss0-dev/traced-web-terminal/internal/runtime"
)

func main() {
	configPath := flag.String("config", "config.json", "path to GW configuration")
	addr := flag.String("addr", ":8081", "GW listen address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := runtime.LoadConfig(*configPath)
	if err != nil {
		logger.Error("load config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	if !runtime.KVMAvailable() {
		logger.Error("KVM acceleration is required but /dev/kvm is not accessible",
			slog.String("hint", "add your user to the kvm group (then start from a fresh shell) or grant access to /dev/kvm"))
		os.Exit(1)
	}

	rt := runtime.NewQEMU(cfg, runtime.Options{})
	logger.Info("launching fleet", slog.Int("targets", len(cfg.Targets)))
	if err := rt.EnsureStarted(); err != nil {
		logger.Error("ensure started", slog.String("error", err.Error()))
		os.Exit(1)
	}

	srv := &http.Server{Addr: *addr, Handler: gw.NewServer(rt).Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("gateway listening", slog.String("addr", *addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve", slog.String("error", err.Error()))
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", slog.String("error", err.Error()))
	}
	if err := rt.Shutdown(); err != nil {
		logger.Error("runtime shutdown", slog.String("error", err.Error()))
	}
	logger.Info("fleet stopped")
}
