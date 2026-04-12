// Command beacon-bench spawns a beacon instance, drives a configurable
// write rate at it for a fixed duration, and asserts that the server's
// resident memory and CPU usage stay inside the budget defined by the
// bootstrap pitch's card 11:
//
//	p95 RSS  < 100 MB
//	peak CPU < 25% of one core
//
// It's a standalone CLI, not a Go test — beacon runs as a real subprocess
// and we measure its /proc state via gopsutil. Exits non-zero on threshold
// breach or on any failure along the way; CI consumes the exit code.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

type config struct {
	Duration    time.Duration
	Rate        int
	MaxRSSMB    float64
	MaxCPUPct   float64
	BeaconBin   string
	BeaconPort  int
	MCPPort     int
	PostgresURL string // if set, use PG instead of SQLite
	KeepDB      bool
}

func main() {
	var cfg config
	flag.DurationVar(&cfg.Duration, "duration", 60*time.Second, "sustained write duration")
	flag.IntVar(&cfg.Rate, "rate", 100, "target events per second")
	flag.Float64Var(&cfg.MaxRSSMB, "max-rss-mb", 100, "fail if p95 RSS exceeds this many MB")
	flag.Float64Var(&cfg.MaxCPUPct, "max-cpu-pct", 25, "fail if peak CPU exceeds this percent of one core")
	flag.StringVar(&cfg.BeaconBin, "binary", "", "path to the beacon binary (defaults to ./beacon or $PATH lookup)")
	flag.IntVar(&cfg.BeaconPort, "port", 14690, "port for beacon HTTP")
	flag.IntVar(&cfg.MCPPort, "mcp-port", 14691, "port for beacon MCP")
	flag.StringVar(&cfg.PostgresURL, "postgres-url", "", "if set, bench against this Postgres instance instead of SQLite")
	flag.BoolVar(&cfg.KeepDB, "keep-db", false, "do not delete the temp SQLite database on exit")
	flag.Parse()

	if cfg.BeaconBin == "" {
		cfg.BeaconBin = resolveBeaconBinary()
	}
	if cfg.BeaconBin == "" {
		fmt.Fprintln(os.Stderr, "beacon-bench: could not find the beacon binary; pass -binary")
		os.Exit(2)
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "beacon-bench: %v\n", err)
		os.Exit(1)
	}
}

func resolveBeaconBinary() string {
	// Prefer a sibling ./beacon in the same dir as beacon-bench — that's the
	// common local-dev layout where both binaries live in /tmp or ./bin.
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "beacon")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	if p, err := exec.LookPath("beacon"); err == nil {
		return p
	}
	return ""
}

func run(cfg config) error {
	tmpDir, err := os.MkdirTemp("", "beacon-bench-*")
	if err != nil {
		return err
	}
	if !cfg.KeepDB {
		defer os.RemoveAll(tmpDir)
	}

	configPath := filepath.Join(tmpDir, "beacon.yml")
	dbPath := filepath.Join(tmpDir, "beacon.db")
	if err := writeBeaconConfig(configPath, dbPath, cfg); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, cfg.BeaconBin, "serve", "-config", configPath)
	// Stream beacon's stdout/stderr into ours so failures are visible.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn beacon: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		_ = cmd.Wait()
	}()

	if err := waitForHealth(cfg.BeaconPort, 10*time.Second); err != nil {
		return fmt.Errorf("beacon never became healthy: %w", err)
	}

	proc, err := process.NewProcess(int32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("attach to beacon pid: %w", err)
	}
	// Prime the CPU counter — gopsutil's CPUPercent needs a prior call to
	// anchor the delta.
	_, _ = proc.CPUPercent()
	time.Sleep(100 * time.Millisecond)

	var wg sync.WaitGroup
	statsCh := make(chan sampleSet, 1)
	samplerCtx, stopSampler := context.WithCancel(context.Background())
	wg.Add(1)
	go func() {
		defer wg.Done()
		statsCh <- sampleLoop(samplerCtx, proc)
	}()

	loadStart := time.Now()
	sent, loadErr := generateLoad(cfg)
	loadDuration := time.Since(loadStart)
	stopSampler()
	wg.Wait()
	stats := <-statsCh

	if loadErr != nil {
		return fmt.Errorf("load generator: %w", loadErr)
	}

	sustainedRate := float64(sent) / loadDuration.Seconds()
	fmt.Printf("\n=== beacon-bench results ===\n")
	fmt.Printf("  events sent:       %d\n", sent)
	fmt.Printf("  duration:          %s\n", loadDuration.Round(time.Millisecond))
	fmt.Printf("  sustained rate:    %.1f events/sec\n", sustainedRate)
	fmt.Printf("  samples collected: %d\n", len(stats.rss))
	fmt.Printf("  p50 RSS:           %.1f MB\n", stats.p50RSS())
	fmt.Printf("  p95 RSS:           %.1f MB\n", stats.p95RSS())
	fmt.Printf("  peak RSS:          %.1f MB\n", stats.peakRSS())
	fmt.Printf("  mean CPU:          %.1f%%\n", stats.meanCPU())
	fmt.Printf("  peak CPU:          %.1f%%\n", stats.peakCPU())
	fmt.Printf("  thresholds:        p95 RSS < %.0f MB, peak CPU < %.0f%%\n", cfg.MaxRSSMB, cfg.MaxCPUPct)

	var failed []string
	if stats.p95RSS() > cfg.MaxRSSMB {
		failed = append(failed, fmt.Sprintf("p95 RSS %.1f MB exceeds budget %.0f MB", stats.p95RSS(), cfg.MaxRSSMB))
	}
	if stats.peakCPU() > cfg.MaxCPUPct {
		failed = append(failed, fmt.Sprintf("peak CPU %.1f%% exceeds budget %.0f%%", stats.peakCPU(), cfg.MaxCPUPct))
	}
	if len(failed) > 0 {
		for _, f := range failed {
			fmt.Fprintf(os.Stderr, "FAIL: %s\n", f)
		}
		return fmt.Errorf("bench failed: %d threshold breach(es)", len(failed))
	}
	// Also fail if we couldn't sustain close to the requested rate — a
	// silently-throttled run would otherwise look clean.
	if sustainedRate < float64(cfg.Rate)*0.8 {
		return fmt.Errorf("sustained rate %.1f/sec under 80%% of target %d/sec", sustainedRate, cfg.Rate)
	}
	fmt.Println("\nPASS")
	return nil
}

func writeBeaconConfig(path, dbPath string, cfg config) error {
	var body string
	if cfg.PostgresURL != "" {
		body = fmt.Sprintf(`server:
  http_port: %d
  mcp_port: %d
database:
  adapter: postgres
  url: %s
rollup:
  tick_seconds: 5
`, cfg.BeaconPort, cfg.MCPPort, cfg.PostgresURL)
	} else {
		body = fmt.Sprintf(`server:
  http_port: %d
  mcp_port: %d
database:
  adapter: sqlite
  path: %s
rollup:
  tick_seconds: 5
`, cfg.BeaconPort, cfg.MCPPort, dbPath)
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

func waitForHealth(port int, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/healthz", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s", timeout)
}

// generateLoad posts events at the target rate. Returns the count of
// successful 2xx posts and the first error (if any).
func generateLoad(cfg config) (int, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/events", cfg.BeaconPort)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()

	interval := time.Second / time.Duration(cfg.Rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var sent int
	for {
		select {
		case <-ctx.Done():
			return sent, nil
		case <-ticker.C:
			if err := postOne(client, url, sent); err != nil {
				return sent, err
			}
			sent++
		}
	}
}

func postOne(client *http.Client, url string, seq int) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	body, _ := json.Marshal(map[string]any{
		"events": []map[string]any{{
			"kind":       "outcome",
			"name":       "bench.event",
			"created_at": now,
			"properties": map[string]any{"seq": seq},
		}},
	})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post #%d: %w", seq, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("post #%d: HTTP %d", seq, resp.StatusCode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sampling
// ---------------------------------------------------------------------------

type sampleSet struct {
	rss []float64 // MB
	cpu []float64 // percent of one core (gopsutil convention)
}

func (s sampleSet) p50RSS() float64  { return percentile(s.rss, 0.50) }
func (s sampleSet) p95RSS() float64  { return percentile(s.rss, 0.95) }
func (s sampleSet) peakRSS() float64 { return peak(s.rss) }
func (s sampleSet) meanCPU() float64 { return mean(s.cpu) }
func (s sampleSet) peakCPU() float64 { return peak(s.cpu) }

// sampleLoop polls the target process every second until ctx is cancelled.
// Missed samples are skipped — we never block the loop on a slow metric read.
func sampleLoop(ctx context.Context, proc *process.Process) sampleSet {
	var s sampleSet
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return s
		case <-t.C:
			if m, err := proc.MemoryInfo(); err == nil && m != nil {
				s.rss = append(s.rss, float64(m.RSS)/1024/1024)
			}
			if c, err := proc.CPUPercent(); err == nil {
				s.cpu = append(s.cpu, c)
			}
		}
	}
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func peak(xs []float64) float64 {
	var m float64
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
