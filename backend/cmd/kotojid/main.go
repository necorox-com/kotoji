// Command kotojid is the kotoji backend server. A single binary serves two
// planes selected by KOTOJI_RUN_MODE: the control plane (REST API + auth + MCP,
// default :8080) and the data plane (static hosting, default :8081). It runs
// with zero external services for RUN_MODE=control in development (health only).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/necorox-com/kotoji/backend/internal/app"
	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/observability"
)

// shutdownTimeout bounds graceful shutdown so a stuck connection can't hang exit.
const shutdownTimeout = 15 * time.Second

func main() {
	if err := run(); err != nil {
		// Logger may not exist yet on a config failure; use a plain stderr line.
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

// run wires the app and blocks until shutdown, returning the first fatal error.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := observability.NewLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	// signal.NotifyContext gives a context cancelled on SIGINT/SIGTERM. It is
	// established BEFORE app.New so a slow boot (OIDC discovery, DB connect) is
	// itself interruptible by a shutdown signal.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// app.New does network I/O (DB ping; OIDC discovery in oidc mode), so it takes
	// the (signal-aware) context and can fail fast on a dead dependency.
	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if cerr := application.Close(closeCtx); cerr != nil {
			logger.Error("close failed", slog.Any("error", cerr))
		}
	}()

	// Build the http.Servers selected by run mode. Control/ServeRouter return nil
	// for a plane this mode does not serve, so we guard against a nil handler.
	var servers []*http.Server
	if h := application.ControlRouter(); h != nil {
		servers = append(servers, &http.Server{
			Addr:              cfg.ControlAddr,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
		})
	}
	if h := application.ServeRouter(); h != nil {
		servers = append(servers, &http.Server{
			Addr:              cfg.ServeAddr,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
		})
	}

	logger.Info("starting kotojid",
		slog.String("env", string(cfg.Env)),
		slog.String("run_mode", string(cfg.Mode)),
		slog.Bool("control", cfg.ServesControl()),
		slog.Bool("serve", cfg.ServesData()),
	)

	// Launch the background operability scheduler (reaper, gc, startup consistency)
	// for control|all modes. It runs under the signal-aware context so a shutdown
	// signal drains it; Close (deferred above) also stops it.
	application.StartBackground(ctx)

	return serve(ctx, logger, servers)
}

// serve starts every server and shuts them all down gracefully on ctx
// cancellation or the first ListenAndServe failure.
func serve(ctx context.Context, logger *slog.Logger, servers []*http.Server) error {
	// errCh carries the first non-graceful server error.
	errCh := make(chan error, len(servers))
	var wg sync.WaitGroup

	for _, srv := range servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			logger.Info("listening", slog.String("addr", s.Addr))
			// ListenAndServe returns ErrServerClosed on graceful Shutdown; that
			// is the expected path, not a failure.
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(srv)
	}

	// Block until a shutdown signal or a server crashes.
	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case runErr = <-errCh:
		logger.Error("server error, shutting down", slog.Any("error", runErr))
	}

	// Gracefully drain all servers within the timeout budget.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", slog.String("addr", srv.Addr), slog.Any("error", err))
			if runErr == nil {
				runErr = err
			}
		}
	}

	wg.Wait()
	return runErr
}
