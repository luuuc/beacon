package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunNoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(nil, &out, &errb)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Usage:") {
		t.Errorf("stderr missing usage: %q", errb.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"banana"}, &out, &errb)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("stderr missing unknown command message: %q", errb.String())
	}
}

func TestRunStubSubcommands(t *testing.T) {
	// mcp is still a stub; it errors "not implemented" until the MCP card.
	var out, errb bytes.Buffer
	code := run([]string{"mcp"}, &out, &errb)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "not implemented") {
		t.Errorf("stderr missing 'not implemented': %q", errb.String())
	}
}

func TestRunRollupBaselinesRequireSubcommand(t *testing.T) {
	// rollup and baselines are now real but require a sub-subcommand.
	for _, name := range []string{"rollup", "baselines"} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run([]string{name}, &out, &errb)
			if code != 2 {
				t.Errorf("code = %d, want 2", code)
			}
			if !strings.Contains(errb.String(), "subcommand required") {
				t.Errorf("stderr missing 'subcommand required': %q", errb.String())
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"version"}, &out, &errb)
	if code != 0 {
		t.Errorf("code = %d, want 0 (stderr=%q)", code, errb.String())
	}
	if !strings.Contains(out.String(), "beacon ") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestServeLoopbackGuardRejectsPublicBind(t *testing.T) {
	clearBeaconEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "beacon.yml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  bind: 0.0.0.0\n  http_port: 14680\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := run([]string{"serve", "-config", cfgPath}, &out, &errb)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	combined := errb.String() + out.String()
	if !strings.Contains(combined, "auth.token") {
		t.Errorf("expected loud auth.token error, got: %q", combined)
	}
}

func clearBeaconEnv(t *testing.T) {
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
