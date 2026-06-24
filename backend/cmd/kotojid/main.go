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
	"github.com/necorox-com/kotoji/backend/internal/tlsedge"
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
		slog.String("tls_mode", string(cfg.TLSMode)),
	)

	// kotoji-native on-demand TLS (KOTOJI_TLS_MODE=auto, §4.5 third deploy mode).
	// Off mode (the DEFAULT) returns (nil, nil) and the binary keeps 100% of its
	// existing plain-HTTP behavior. Auto mode adds a :443 listener (the combined
	// Host-routing handler over CertMagic on-demand certs) + a :80 listener (ACME
	// HTTP-01 + HTTPS redirect) ON TOP of the existing :8080/:8081 servers, which
	// stay bound for internal health checks behind the new TLS edge.
	tlsEngine, err := application.NewTLSEngine()
	if err != nil {
		return err
	}

	// Launch the background operability scheduler (reaper, gc, startup consistency)
	// for control|all modes. It runs under the signal-aware context so a shutdown
	// signal drains it; Close (deferred above) also stops it.
	application.StartBackground(ctx)

	return serve(ctx, logger, servers, tlsEngine)
}

// serve starts every server (plus the optional kotoji-native TLS engine) and shuts
// them all down gracefully on ctx cancellation or the first listener failure.
// tlsEngine is nil unless KOTOJI_TLS_MODE=auto; when nil the TLS path is wholly
// inert and behavior is identical to before.
func serve(ctx context.Context, logger *slog.Logger, servers []*http.Server, tlsEngine *tlsedge.Engine) error {
	// errCh carries the first non-graceful error from any server OR the TLS engine.
	errCh := make(chan error, len(servers)+1)
	var wg sync.WaitGroup

	// engineCtx lets us stop the TLS engine deterministically: its own Run drains on
	// ctx-cancel, but a plain-server crash must also bring it down, so we cancel this
	// derived context in the shutdown path below.
	engineCtx, cancelEngine := context.WithCancel(ctx)
	defer cancelEngine()

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

	// The TLS engine owns its own :443/:80 listeners and graceful drain; it blocks
	// until engineCtx is cancelled or one of its listeners fails.
	if tlsEngine != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tlsEngine.Run(engineCtx); err != nil {
				errCh <- err
			}
		}()
	}

	// Block until a shutdown signal or a server/engine crashes.
	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case runErr = <-errCh:
		logger.Error("server error, shutting down", slog.Any("error", runErr))
	}

	// Bring the TLS engine down too (no-op when nil); its Run returns on cancel.
	cancelEngine()

	// Gracefully drain all plain servers within the timeout budget.
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
