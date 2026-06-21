// Command kotoji-ops runs the kotoji background operability jobs ONCE for manual or
// cron invocation (architecture.md §8.4): the soft-delete reaper, opportunistic
// `git gc --auto`, and the startup consistency check. It shares the same config +
// store + ops package the in-process scheduler uses, so a cron run and the
// embedded scheduler behave identically.
//
// Usage (env-configured, same KOTOJI_* vars as kotojid):
//
//	kotoji-ops [reap|gc|check|all]   (default: all)
//
// It is safe to run alongside kotojid in single-replica deployments because every
// destructive step is per-site flock-guarded inside the git layer and the reaper
// only acts on PAST-GRACE rows; still, prefer running it when the scheduler is off
// (e.g. RUN_MODE=serve replicas) to avoid duplicated work.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/necorox-com/kotoji/backend/internal/config"
	"github.com/necorox-com/kotoji/backend/internal/db"
	"github.com/necorox-com/kotoji/backend/internal/observability"
	"github.com/necorox-com/kotoji/backend/internal/ops"
)

// sitesSubdir mirrors app.sitesSubdir: per-site repos live under DATA_DIR/sites.
const sitesSubdir = "sites"

func main() {
	if err := run(); err != nil {
		slog.Error("kotoji-ops fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := observability.NewLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	o := ops.New(ops.Config{
		SitesDir:     filepath.Join(cfg.DataDir, sitesSubdir),
		BackupDir:    cfg.BackupDir,
		Grace:        cfg.SoftDeleteGrace,
		Interval:     cfg.OpsInterval,
		EnableReaper: true,
		EnableGC:     true,
	}, opsStore{store}, ops.NewExecGitRunner(cfg.GitBin), ops.OSFS{}, logger)

	job := "all"
	if len(os.Args) > 1 {
		job = os.Args[1]
	}

	switch job {
	case "reap":
		return reap(ctx, o, logger)
	case "gc":
		return o.GC(ctx)
	case "check":
		_, cerr := o.Consistency(ctx)
		return cerr
	case "all":
		if _, cerr := o.Consistency(ctx); cerr != nil {
			logger.Warn("consistency check failed", slog.Any("error", cerr))
		}
		if rerr := reap(ctx, o, logger); rerr != nil {
			logger.Warn("reaper failed", slog.Any("error", rerr))
		}
		return o.GC(ctx)
	default:
		return fmt.Errorf("unknown job %q (want reap|gc|check|all)", job)
	}
}

// reap runs one reaper pass and logs the result summary.
func reap(ctx context.Context, o *ops.Ops, logger *slog.Logger) error {
	res, err := o.Reap(ctx)
	if err != nil {
		return err
	}
	logger.Info("reaper result", slog.Int("reaped", res.Reaped), slog.Int("skipped", res.Skipped), slog.Int("errors", len(res.Errors)))
	for _, e := range res.Errors {
		logger.Warn("reaper site error", slog.String("detail", e))
	}
	return nil
}
