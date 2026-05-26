package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Bind != "127.0.0.1" {
		t.Errorf("bind = %q, want 127.0.0.1", cfg.Server.Bind)
	}
	if cfg.Server.HTTPPort != 4680 {
		t.Errorf("http_port = %d, want 4680", cfg.Server.HTTPPort)
	}
	if cfg.Rollup.TickSeconds != 60 {
		t.Errorf("rollup.tick_seconds = %d, want 60", cfg.Rollup.TickSeconds)
	}
	if len(cfg.Baseline.Windows) != 3 {
		t.Errorf("baseline.windows = %v, want 3 entries", cfg.Baseline.Windows)
	}
}

func TestLoadFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.yml")
	if err := os.WriteFile(path, []byte(`
server:
  bind: 0.0.0.0
  http_port: 9999
  auth:
    token: secret
database:
  url: postgres://example/db
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Bind != "0.0.0.0" {
		t.Errorf("bind = %q", cfg.Server.Bind)
	}
	if cfg.Server.HTTPPort != 9999 {
		t.Errorf("http_port = %d", cfg.Server.HTTPPort)
	}
	if cfg.Server.Auth.Token != "secret" {
		t.Errorf("auth.token = %q", cfg.Server.Auth.Token)
	}
	if cfg.Database.URL != "postgres://example/db" {
		t.Errorf("database.url = %q", cfg.Database.URL)
	}
	// MCPPort is deprecated — no default.
	if cfg.Server.MCPPortSet {
		t.Errorf("mcp_port_set should be false when not specified")
	}
}

func TestLoadEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("BEACON_HTTP_PORT", "7777")
	t.Setenv("BEACON_BIND", "192.0.2.1")
	t.Setenv("BEACON_AUTH_TOKEN", "envtoken")
	t.Setenv("BEACON_DATABASE_URL", "sqlite:///tmp/x.db")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.HTTPPort != 7777 {
		t.Errorf("http_port = %d", cfg.Server.HTTPPort)
	}
	if cfg.Server.Bind != "192.0.2.1" {
		t.Errorf("bind = %q", cfg.Server.Bind)
	}
	if cfg.Server.Auth.Token != "envtoken" {
		t.Errorf("auth.token = %q", cfg.Server.Auth.Token)
	}
	if cfg.Database.URL != "sqlite:///tmp/x.db" {
		t.Errorf("database.url = %q", cfg.Database.URL)
	}
}

func TestValidateLoopbackGuard(t *testing.T) {
	cases := []struct {
		name    string
		bind    string
		token   string
		wantErr bool
	}{
		{"loopback no token ok", "127.0.0.1", "", false},
		{"localhost no token ok", "localhost", "", false},
		{"ipv6 loopback no token ok", "::1", "", false},
		{"public no token rejected", "0.0.0.0", "", true},
		{"public with token ok", "0.0.0.0", "s", false},
		{"private no token rejected", "10.0.0.5", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Server.Bind = tc.bind
			cfg.Server.Auth.Token = tc.token
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "auth.token") {
				t.Errorf("error missing auth.token hint: %v", err)
			}
		})
	}
}

func TestValidatePprofLoopbackOnly(t *testing.T) {
	cases := []struct {
		name    string
		bind    string
		token   string
		pprof   bool
		wantErr bool
	}{
		{"pprof on loopback ok", "127.0.0.1", "", true, false},
		{"pprof off on public ok", "10.0.0.5", "s", false, false},
		{"pprof on public with token rejected", "10.0.0.5", "s", true, true},
		{"pprof on 0.0.0.0 with token rejected", "0.0.0.0", "s", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Server.Bind = tc.bind
			cfg.Server.Auth.Token = tc.token
			cfg.Server.PprofEnabled = tc.pprof
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "pprof") {
				t.Errorf("error missing pprof hint: %v", err)
			}
		})
	}
}

// TestEnvDatabaseURLOverridesYAMLAdapter pins the v0.2.1 fix for the
// bug that shipped Beacon running embedded SQLite on Maket's staging
// deploy. A YAML file sets `adapter: sqlite` (the historical baked
// default inside the Docker image), env sets BEACON_DATABASE_URL to a
// postgres URL — the postgres URL must win so the deployed accessory
// actually uses the intended database. Without this, the user sees
// /readyz=ok while Beacon silently writes to a SQLite file nobody
// queries.
func TestEnvDatabaseURLOverridesYAMLAdapter(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.yml")
	if err := os.WriteFile(path, []byte(`
database:
  adapter: sqlite
  path: /var/lib/beacon/beacon.db
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEACON_DATABASE_URL", "postgres://maket:pw@maket-db:5432/beacon_staging")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.URL != "postgres://maket:pw@maket-db:5432/beacon_staging" {
		t.Errorf("URL = %q, want postgres URL from env", cfg.Database.URL)
	}
	// Adapter field stays as the YAML value — resolveKind's URL-first
	// precedence is the code path that handles the tie-break, not a
	// config-time mutation. Exercise ResolveKind indirectly via
	// adapterfactory to confirm the full chain.
	if cfg.Database.Adapter != "sqlite" {
		t.Errorf("Adapter = %q, want sqlite (from YAML)", cfg.Database.Adapter)
	}
}

func TestEnvDatabaseAdapterAndPathMapping(t *testing.T) {
	clearEnv(t)
	t.Setenv("BEACON_DATABASE_ADAPTER", "sqlite")
	t.Setenv("BEACON_DATABASE_PATH", "/var/lib/beacon/beacon.db")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Adapter != "sqlite" {
		t.Errorf("Adapter = %q, want sqlite", cfg.Database.Adapter)
	}
	if cfg.Database.Path != "/var/lib/beacon/beacon.db" {
		t.Errorf("Path = %q, want /var/lib/beacon/beacon.db", cfg.Database.Path)
	}
}

func TestLoadBadYAML(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.yml")
	if err := os.WriteFile(path, []byte("server: [not a map"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"BEACON_BIND", "BEACON_HTTP_PORT", "BEACON_MCP_PORT",
		"BEACON_AUTH_TOKEN", "BEACON_PPROF_ENABLED",
		"BEACON_DATABASE_URL", "BEACON_DATABASE_ADAPTER",
		"BEACON_DATABASE_PATH", "BEACON_DATABASE_SCHEMA",
		"BEACON_DATABASE_MAX_CONNS",
		"BEACON_INGEST_TRUST_XFF", "BEACON_INGEST_IDEMP_MAX_ENTRIES",
		"BEACON_FILTER_EXCLUDE_PATHS", "BEACON_FILTER_DEFAULTS",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestFilterConfigFromYAML(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.yml")
	if err := os.WriteFile(path, []byte(`
filter:
  exclude_paths:
    - "*.php"
    - "/custom/*"
  defaults: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Filter.ExcludePaths) != 2 {
		t.Fatalf("exclude_paths = %v, want 2 entries", cfg.Filter.ExcludePaths)
	}
	if cfg.Filter.Defaults == nil || *cfg.Filter.Defaults != false {
		t.Errorf("defaults = %v, want false", cfg.Filter.Defaults)
	}
}

func TestFilterConfigEnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("BEACON_FILTER_EXCLUDE_PATHS", "/probe/*,*.aspx")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Filter.ExcludePaths) != 2 {
		t.Fatalf("exclude_paths = %v, want 2", cfg.Filter.ExcludePaths)
	}
	if cfg.Filter.ExcludePaths[0] != "/probe/*" {
		t.Errorf("exclude_paths[0] = %q, want /probe/*", cfg.Filter.ExcludePaths[0])
	}
}

func TestFilterConfigEnvDefaultsFalse(t *testing.T) {
	clearEnv(t)
	t.Setenv("BEACON_FILTER_DEFAULTS", "false")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Filter.Defaults == nil || *cfg.Filter.Defaults != false {
		t.Errorf("defaults = %v, want false", cfg.Filter.Defaults)
	}
	f := NewPathFilter(cfg.Filter)
	if f.ShouldExclude("/foo.php") {
		t.Error("expected *.php to NOT be excluded when defaults disabled via env")
	}
}

func TestValidateRejectsBadGlobPattern(t *testing.T) {
	cfg := Defaults()
	cfg.Filter.ExcludePaths = []string{"[invalid"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for bad glob pattern")
	}
	if !strings.Contains(err.Error(), "invalid glob pattern") {
		t.Errorf("error = %v, want 'invalid glob pattern' hint", err)
	}
}

func TestPathFilterDefaultPatterns(t *testing.T) {
	f := NewPathFilter(FilterConfig{})
	cases := []struct {
		path    string
		exclude bool
	}{
		{"/wp-class.php", true},
		{"/sadcut1.php", true},
		{"/wp-admin", true},
		{"/wp-content", true},
		{"/cgi-bin", true},
		{"/xmlrpc", true},
		{"/items/123", false},
		{"/search", false},
		{"/up", false},
		{"/api/events", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := f.ShouldExclude(tc.path)
			if got != tc.exclude {
				t.Errorf("ShouldExclude(%q) = %v, want %v", tc.path, got, tc.exclude)
			}
		})
	}
}

func TestPathFilterUserPatternsExtendDefaults(t *testing.T) {
	f := NewPathFilter(FilterConfig{
		ExcludePaths: []string{"/up", "/health"},
	})
	// User patterns
	if !f.ShouldExclude("/up") {
		t.Error("expected /up to be excluded (user pattern)")
	}
	if !f.ShouldExclude("/health") {
		t.Error("expected /health to be excluded (user pattern)")
	}
	// Defaults still active
	if !f.ShouldExclude("/lock360.php") {
		t.Error("expected *.php default to still be active")
	}
}

func TestPathFilterDefaultsDisabled(t *testing.T) {
	no := false
	f := NewPathFilter(FilterConfig{
		ExcludePaths: []string{"/up"},
		Defaults:     &no,
	})
	if !f.ShouldExclude("/up") {
		t.Error("expected /up to be excluded")
	}
	if f.ShouldExclude("/lock360.php") {
		t.Error("expected *.php to NOT be excluded when defaults disabled")
	}
}

func TestParseBeaconDuration(t *testing.T) {
	cases := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"14d", 14 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"0d", 0, false},
		{"24h", 24 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"500ms", 500 * time.Millisecond, false},
		{"", 0, true},
		{"xd", 0, true},
		{"notaduration", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseBeaconDuration(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("ParseBeaconDuration(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestDefaultExcludePaths(t *testing.T) {
	paths := DefaultExcludePaths()
	if len(paths) != 4 {
		t.Fatalf("len = %d, want 4", len(paths))
	}
	// Verify it's a copy — mutating the result shouldn't affect subsequent calls.
	paths[0] = "mutated"
	fresh := DefaultExcludePaths()
	if fresh[0] == "mutated" {
		t.Error("DefaultExcludePaths returned a reference, not a copy")
	}
}

func TestPathFilterPatterns(t *testing.T) {
	f := NewPathFilter(FilterConfig{ExcludePaths: []string{"/custom/*"}})
	pats := f.Patterns()
	if len(pats) != 5 { // 4 defaults + 1 custom
		t.Fatalf("len = %d, want 5", len(pats))
	}
	// Verify it's a copy.
	pats[0] = "mutated"
	fresh := f.Patterns()
	if fresh[0] == "mutated" {
		t.Error("Patterns returned a reference, not a copy")
	}
}

func TestApplyEnvErrors(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad http port", "BEACON_HTTP_PORT", "abc"},
		{"bad mcp port", "BEACON_MCP_PORT", "abc"},
		{"bad pprof", "BEACON_PPROF_ENABLED", "abc"},
		{"bad max conns", "BEACON_DATABASE_MAX_CONNS", "abc"},
		{"bad trust xff", "BEACON_INGEST_TRUST_XFF", "abc"},
		{"bad idemp entries", "BEACON_INGEST_IDEMP_MAX_ENTRIES", "abc"},
		{"bad filter defaults", "BEACON_FILTER_DEFAULTS", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tc.key, tc.val)
			_, err := Load("")
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestApplyEnvMCPPort(t *testing.T) {
	clearEnv(t)
	t.Setenv("BEACON_MCP_PORT", "5555")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.MCPPort != 5555 {
		t.Errorf("mcp_port = %d, want 5555", cfg.Server.MCPPort)
	}
	if !cfg.Server.MCPPortSet {
		t.Error("MCPPortSet should be true")
	}
}

func TestApplyEnvPprofEnabled(t *testing.T) {
	clearEnv(t)
	t.Setenv("BEACON_PPROF_ENABLED", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Server.PprofEnabled {
		t.Error("pprof_enabled should be true")
	}
}

func TestApplyEnvIngestFields(t *testing.T) {
	clearEnv(t)
	t.Setenv("BEACON_INGEST_TRUST_XFF", "true")
	t.Setenv("BEACON_INGEST_IDEMP_MAX_ENTRIES", "5000")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Ingest.TrustXFF {
		t.Error("trust_xff should be true")
	}
	if cfg.Ingest.IdempMaxEntries != 5000 {
		t.Errorf("idemp_max_entries = %d, want 5000", cfg.Ingest.IdempMaxEntries)
	}
}

func TestApplyEnvDatabaseSchema(t *testing.T) {
	clearEnv(t)
	t.Setenv("BEACON_DATABASE_SCHEMA", "myschema")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Schema != "myschema" {
		t.Errorf("schema = %q, want myschema", cfg.Database.Schema)
	}
}

func TestApplyEnvMaxConns(t *testing.T) {
	clearEnv(t)
	t.Setenv("BEACON_DATABASE_MAX_CONNS", "25")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.MaxConns != 25 {
		t.Errorf("max_conns = %d, want 25", cfg.Database.MaxConns)
	}
}

func TestMergeNonZeroAllFields(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.yml")
	if err := os.WriteFile(path, []byte(`
server:
  bind: 10.0.0.1
  http_port: 8080
  mcp_port: 9090
  pprof_enabled: true
  auth:
    token: s3cret
database:
  url: postgres://localhost/db
  adapter: postgres
  path: /tmp/db
  schema: public
  max_conns: 50
retention:
  events_days: 30
  ambient_retention_hours: 48
  rollups_hour_days: 180
rollup:
  tick_seconds: 120
  prune_at: "04:00"
  timezone: America/New_York
baseline:
  windows: ["12h", "3d"]
ingest:
  trust_xff: true
  idemp_max_entries: 200000
ambient:
  anomaly:
    baseline_window: "7d"
    detection_window: "12h"
    sigma_threshold: 2.5
    min_volume: 20
filter:
  exclude_paths: ["/health"]
  defaults: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.MCPPort != 9090 {
		t.Errorf("mcp_port = %d, want 9090", cfg.Server.MCPPort)
	}
	if !cfg.Server.MCPPortSet {
		t.Error("MCPPortSet should be true from YAML")
	}
	if cfg.Retention.EventsDays != 30 {
		t.Errorf("events_days = %d, want 30", cfg.Retention.EventsDays)
	}
	if cfg.Retention.AmbientRetentionHours != 48 {
		t.Errorf("ambient_retention_hours = %d, want 48", cfg.Retention.AmbientRetentionHours)
	}
	if cfg.Retention.RollupsHourDays != 180 {
		t.Errorf("rollups_hour_days = %d, want 180", cfg.Retention.RollupsHourDays)
	}
	if cfg.Rollup.TickSeconds != 120 {
		t.Errorf("tick_seconds = %d, want 120", cfg.Rollup.TickSeconds)
	}
	if cfg.Rollup.PruneAt != "04:00" {
		t.Errorf("prune_at = %q, want 04:00", cfg.Rollup.PruneAt)
	}
	if cfg.Rollup.Timezone != "America/New_York" {
		t.Errorf("timezone = %q", cfg.Rollup.Timezone)
	}
	if len(cfg.Baseline.Windows) != 2 || cfg.Baseline.Windows[0] != "12h" {
		t.Errorf("baseline.windows = %v", cfg.Baseline.Windows)
	}
	if !cfg.Ingest.TrustXFF {
		t.Error("trust_xff should be true")
	}
	if cfg.Ingest.IdempMaxEntries != 200000 {
		t.Errorf("idemp_max_entries = %d", cfg.Ingest.IdempMaxEntries)
	}
	if cfg.Ambient.Anomaly.BaselineWindow != "7d" {
		t.Errorf("baseline_window = %q", cfg.Ambient.Anomaly.BaselineWindow)
	}
	if cfg.Ambient.Anomaly.DetectionWindow != "12h" {
		t.Errorf("detection_window = %q", cfg.Ambient.Anomaly.DetectionWindow)
	}
	if cfg.Ambient.Anomaly.SigmaThreshold != 2.5 {
		t.Errorf("sigma_threshold = %f", cfg.Ambient.Anomaly.SigmaThreshold)
	}
	if cfg.Ambient.Anomaly.MinVolume != 20 {
		t.Errorf("min_volume = %d", cfg.Ambient.Anomaly.MinVolume)
	}
	if cfg.Database.Schema != "public" {
		t.Errorf("schema = %q", cfg.Database.Schema)
	}
	if cfg.Database.MaxConns != 50 {
		t.Errorf("max_conns = %d", cfg.Database.MaxConns)
	}
}

func TestValidatePortRange(t *testing.T) {
	cases := []struct {
		name string
		port int
	}{
		{"zero", 0},
		{"negative", -1},
		{"too high", 70000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Server.HTTPPort = tc.port
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestValidateEmptyBind(t *testing.T) {
	cfg := Defaults()
	cfg.Server.Bind = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty bind")
	}
}

func TestValidateMaxConns(t *testing.T) {
	cfg := Defaults()
	cfg.Database.MaxConns = 201
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for max_conns > 200")
	}
	cfg.Database.MaxConns = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative max_conns")
	}
}

func TestValidateIdempMaxEntries(t *testing.T) {
	cfg := Defaults()
	cfg.Ingest.IdempMaxEntries = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative idemp_max_entries")
	}
}

func TestIsLoopbackBind(t *testing.T) {
	cases := []struct {
		bind string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"localhost", true},
		{"0.0.0.0", false},
		{"10.0.0.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.bind, func(t *testing.T) {
			cfg := Config{Server: ServerConfig{Bind: tc.bind}}
			if got := cfg.IsLoopbackBind(); got != tc.want {
				t.Errorf("IsLoopbackBind() = %v, want %v", got, tc.want)
			}
		})
	}
}
