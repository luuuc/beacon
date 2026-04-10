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
	if cfg.Server.MCPPort != 4681 {
		t.Errorf("mcp_port = %d, want 4681", cfg.Server.MCPPort)
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
	// Defaults survive for unspecified fields.
	if cfg.Server.MCPPort != 4681 {
		t.Errorf("mcp_port default lost: %d", cfg.Server.MCPPort)
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
		"BEACON_DATABASE_URL", "BEACON_DATABASE_SCHEMA",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}
