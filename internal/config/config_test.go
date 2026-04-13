package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}
