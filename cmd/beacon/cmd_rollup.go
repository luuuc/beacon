package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/luuuc/beacon/internal/adapterfactory"
	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/config"
	"github.com/luuuc/beacon/internal/rollup"
)

// cmdRollup dispatches `beacon rollup <subcommand>`.
func cmdRollup(args []string, log *slog.Logger, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "beacon rollup: subcommand required (recompute)")
		return 2
	}
	switch args[0] {
	case "recompute":
		return cmdRollupRecompute(args[1:], log, stderr)
	default:
		fmt.Fprintf(stderr, "beacon rollup: unknown subcommand %q\n", args[0])
		return 2
	}
}

func cmdRollupRecompute(args []string, log *slog.Logger, stderr io.Writer) int {
	fs := flag.NewFlagSet("rollup recompute", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to beacon.yml")
	sinceStr := fs.String("since", "", "recompute window start: a duration (24h, 7d) or an RFC3339 timestamp")
	kindStr := fs.String("kind", "", "narrow scan to a single kind (outcome|perf|error)")
	nameStr := fs.String("name", "", "narrow scan to a single event name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sinceStr == "" {
		fmt.Fprintln(stderr, "beacon rollup recompute: --since is required")
		return 2
	}
	since, err := parseSince(*sinceStr)
	if err != nil {
		fmt.Fprintf(stderr, "beacon rollup recompute: %v\n", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, adapter, err := openAdapterForCLI(ctx, *configPath)
	if err != nil {
		fmt.Fprintf(stderr, "beacon rollup recompute: %v\n", err)
		return 1
	}
	defer func() { _ = adapter.Close() }()

	if err := adapter.Migrate(ctx); err != nil {
		fmt.Fprintf(stderr, "beacon rollup recompute: migrate: %v\n", err)
		return 1
	}

	worker := newWorkerFromConfig(cfg, adapter, log)
	log.Info("recomputing rollups",
		"since", since.Format(time.RFC3339),
		"kind", *kindStr,
		"name", *nameStr,
	)
	if err := worker.RecomputeRange(ctx, since, beacondb.Kind(*kindStr), *nameStr); err != nil {
		fmt.Fprintf(stderr, "beacon rollup recompute: %v\n", err)
		return 1
	}
	log.Info("recompute complete")
	return 0
}

// parseSince accepts either a Go duration ("24h", "7m"), the shorthand "Nd"
// for days, or an RFC3339 timestamp. Durations are resolved against time.Now().
func parseSince(s string) (time.Time, error) {
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, errors.New("unparseable: expected a duration (24h, 7d) or an RFC3339 timestamp")
}

// openAdapterForCLI loads config, validates, and opens the configured
// adapter. Caller is responsible for Close.
func openAdapterForCLI(ctx context.Context, configPath string) (*config.Config, beacondb.Adapter, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	// Skip cfg.Validate here — CLI runs shouldn't require a loopback bind.
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	adapter, err := adapterfactory.Open(openCtx, cfg.Database)
	if err != nil {
		return nil, nil, fmt.Errorf("open adapter: %w", err)
	}
	return cfg, adapter, nil
}

// newWorkerFromConfig builds a rollup.Worker with the same configuration
// the server would use. Used by the CLI subcommands so recomputes honor
// the deployed config.
func newWorkerFromConfig(cfg *config.Config, adapter beacondb.Adapter, log *slog.Logger) *rollup.Worker {
	tz, err := time.LoadLocation(cfg.Rollup.Timezone)
	if err != nil || tz == nil {
		tz = time.UTC
	}
	return rollup.NewWorker(rollup.Config{
		TickInterval: time.Duration(cfg.Rollup.TickSeconds) * time.Second,
		RetentionRaw: time.Duration(cfg.Retention.EventsDays) * 24 * time.Hour,
		PruneAt:      cfg.Rollup.PruneAt,
		Timezone:     tz,
	}, adapter, log)
}
