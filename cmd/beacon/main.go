// Command beacon is the single-binary entry point for the Beacon
// observability accessory. Subcommands: serve, rollup, baselines, mcp.
// Card 1 wires `serve` end-to-end; the other subcommands land in later cards.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/luuuc/beacon/internal/adapterfactory"
	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/config"
	"github.com/luuuc/beacon/internal/ingest"
	"github.com/luuuc/beacon/internal/mcpserver"
	"github.com/luuuc/beacon/internal/reads"
	"github.com/luuuc/beacon/internal/rollup"
	"github.com/luuuc/beacon/internal/server"
	"github.com/luuuc/beacon/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `beacon %s

Usage:
  beacon <command> [flags]

Commands:
  serve       Run the HTTP (and eventually MCP) listeners and rollup worker
  rollup      Run or recompute rollups (not yet implemented)
  baselines   Manage baselines (not yet implemented)
  mcp         Run the MCP server standalone (not yet implemented)

Run 'beacon <command> -h' for command-specific flags.
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, usage, version.Version)
		return 2
	}

	cmd, rest := args[0], args[1:]
	log := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch cmd {
	case "serve":
		return cmdServe(rest, log, stderr)
	case "rollup":
		return cmdRollup(rest, log, stderr)
	case "baselines":
		return cmdBaselines(rest, log, stderr)
	case "mcp":
		fmt.Fprintf(stderr, "beacon %s: not implemented yet (pitch 00 card for this subsystem has not landed)\n", cmd)
		return 2
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "beacon %s\n", version.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprintf(stdout, usage, version.Version)
		return 0
	default:
		fmt.Fprintf(stderr, "beacon: unknown command %q\n\n", cmd)
		fmt.Fprintf(stderr, usage, version.Version)
		return 2
	}
}

func cmdServe(args []string, log *slog.Logger, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to beacon.yml (optional; defaults + env vars are used when empty)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "beacon serve: %v\n", err)
		return 1
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "beacon serve: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Adapter selection + migration are part of boot. If either fails, the
	// binary refuses to start so the supervisor surfaces the error.
	kind, err := adapterfactory.ResolveKind(cfg.Database)
	if err != nil {
		fmt.Fprintf(stderr, "beacon serve: %v\n", err)
		return 1
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, 10*time.Second)
	adapter, err := adapterfactory.Open(openCtx, cfg.Database)
	cancelOpen()
	if err != nil {
		fmt.Fprintf(stderr, "beacon serve: open adapter: %v\n", err)
		return 1
	}
	defer func() { _ = adapter.Close() }()

	migCtx, cancelMig := context.WithTimeout(ctx, 30*time.Second)
	if err := adapter.Migrate(migCtx); err != nil {
		cancelMig()
		fmt.Fprintf(stderr, "beacon serve: migrate: %v\n", err)
		return 1
	}
	cancelMig()

	tz, err := time.LoadLocation(cfg.Rollup.Timezone)
	if err != nil {
		log.Warn("rollup.timezone unknown; falling back to UTC", "value", cfg.Rollup.Timezone, "err", err)
		tz = time.UTC
	}
	worker := rollup.NewWorker(rollup.Config{
		TickInterval: time.Duration(cfg.Rollup.TickSeconds) * time.Second,
		RetentionRaw: time.Duration(cfg.Retention.EventsDays) * 24 * time.Hour,
		PruneAt:      cfg.Rollup.PruneAt,
		Timezone:     tz,
	}, adapter, log)

	log.Info("beacon starting",
		"version", version.Version,
		"bind", cfg.Server.Bind,
		"http_port", cfg.Server.HTTPPort,
		"pprof_enabled", cfg.Server.PprofEnabled,
		"adapter", kind,
		"rollup_tick_seconds", cfg.Rollup.TickSeconds,
		"retention_days", cfg.Retention.EventsDays,
	)

	checks := server.ReadyChecks{
		DBReachable:       func(ctx context.Context) error { return adapter.Ping(ctx) },
		MigrationsApplied: migrationsAppliedCheck(adapter),
		RollupRecent:      server.RollupTickCheck(worker.LastTick, 5*time.Minute),
	}

	srv := server.New(cfg, checks, log)

	ingestH := ingest.NewHandler(ingest.Config{
		AuthToken:       cfg.Server.Auth.Token,
		TrustXFF:        cfg.Ingest.TrustXFF,
		IdempMaxEntries: cfg.Ingest.IdempMaxEntries,
	}, adapter, log)
	srv.Mount("POST /events", ingestH)

	readsH := reads.NewHandler(reads.Config{
		AuthToken: cfg.Server.Auth.Token,
	}, adapter, log)
	readsH.Mount(muxAdapter{srv: srv})

	mcpSrv := mcpserver.New(mcpserver.Config{
		Bind:      cfg.Server.Bind,
		Port:      cfg.Server.MCPPort,
		AuthToken: cfg.Server.Auth.Token,
	}, readsH, worker, log)

	// Rollup worker + HTTP server + MCP server all run alongside each other.
	// They all die when ctx is cancelled. We wait for every goroutine to
	// drain before returning so the deferred adapter.Close is safe.
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		if err := worker.Run(ctx); err != nil {
			log.Error("rollup worker exited with error", "err", err)
		}
	}()

	mcpDone := make(chan struct{})
	go func() {
		defer close(mcpDone)
		if err := mcpSrv.Run(ctx); err != nil {
			log.Error("mcp server exited with error", "err", err)
		}
	}()

	runErr := srv.Run(ctx)
	<-workerDone
	<-mcpDone
	if runErr != nil {
		log.Error("server exited with error", "err", runErr)
		return 1
	}
	log.Info("beacon stopped")
	return 0
}

// muxAdapter bridges reads.Handler.Mount (which wants the minimal mux
// interface) to the Server's Mount method.
type muxAdapter struct{ srv *server.Server }

func (m muxAdapter) Handle(pattern string, h http.Handler) { m.srv.Mount(pattern, h) }

func migrationsAppliedCheck(a beacondb.Adapter) server.ReadyCheck {
	return func(ctx context.Context) error {
		ok, err := a.MigrationsApplied(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("pending migrations")
		}
		return nil
	}
}
