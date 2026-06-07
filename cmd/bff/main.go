// Command bff is the browser-facing front. It serves the static frontend,
// proxies the Gateway target list, and relays WebSocket frames 1:1 between the
// browser and the Gateway. It holds no credentials.
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

	"github.com/abyss0-dev/traced-web-terminal/internal/bff"
)

func main() {
	gwAddr := flag.String("gw", "http://127.0.0.1:8081", "Gateway base URL")
	addr := flag.String("addr", ":8080", "BFF listen address")
	webDir := flag.String("web", "web", "static frontend directory")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	srv := &http.Server{Addr: *addr, Handler: bff.NewServer(*gwAddr, *webDir).Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("bff listening", slog.String("addr", *addr), slog.String("gw", *gwAddr))
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
}
