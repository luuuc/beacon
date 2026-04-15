package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitDefaultPostgres(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out, errb bytes.Buffer
	code := cmdInit([]string{"--database", "postgres"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errb.String())
	}

	assertFileExists(t, filepath.Join(dir, "docker-compose.yml"))
	assertFileExists(t, filepath.Join(dir, ".mcp.json"))
	assertFileNotExists(t, filepath.Join(dir, "config", "initializers", "beacon.rb"))

	if !strings.Contains(out.String(), "Wrote 2 file(s)") {
		t.Errorf("stdout missing write count: %q", out.String())
	}
}

func TestInitWithRuby(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out, errb bytes.Buffer
	code := cmdInit([]string{"--database", "postgres", "--ruby"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errb.String())
	}

	assertFileExists(t, filepath.Join(dir, "docker-compose.yml"))
	assertFileExists(t, filepath.Join(dir, ".mcp.json"))
	assertFileExists(t, filepath.Join(dir, "config", "initializers", "beacon.rb"))

	if !strings.Contains(out.String(), "Wrote 3 file(s)") {
		t.Errorf("stdout missing write count: %q", out.String())
	}
	if !strings.Contains(out.String(), "bundle add beacon-client") {
		t.Errorf("stdout missing ruby next steps: %q", out.String())
	}
}

func TestInitInvalidDatabase(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out, errb bytes.Buffer
	code := cmdInit([]string{"--database", "oracle"}, &out, &errb)
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unsupported database") {
		t.Errorf("stderr missing error: %q", errb.String())
	}
}

func TestInitSkipsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	// Pre-create the files.
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("existing"), 0o644)
	os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("existing"), 0o644)

	var out, errb bytes.Buffer
	code := cmdInit([]string{"--database", "postgres"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errb.String())
	}

	if !strings.Contains(out.String(), "nothing to write") {
		t.Errorf("stdout missing skip message: %q", out.String())
	}
	if !strings.Contains(errb.String(), "skip docker-compose.yml") {
		t.Errorf("stderr missing skip: %q", errb.String())
	}

	// Verify files were not overwritten.
	data, _ := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if string(data) != "existing" {
		t.Errorf("docker-compose.yml was overwritten")
	}
}

func TestInitEndpointFlag(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out, errb bytes.Buffer
	code := cmdInit([]string{"--database", "postgres", "--endpoint", "https://beacon.example.com", "--ruby"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errb.String())
	}

	mcp, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mcp), "https://beacon.example.com/mcp/rpc") {
		t.Errorf(".mcp.json missing custom endpoint: %s", mcp)
	}

	rb, err := os.ReadFile(filepath.Join(dir, "config", "initializers", "beacon.rb"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rb), "https://beacon.example.com") {
		t.Errorf("beacon.rb missing custom endpoint: %s", rb)
	}
}

func TestInitMySQLDSNFormat(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out, errb bytes.Buffer
	code := cmdInit([]string{"--database", "mysql"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errb.String())
	}

	compose, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}

	// Must use Go MySQL DSN format, not mysql:// URL.
	if strings.Contains(string(compose), "mysql://") {
		t.Error("docker-compose.yml uses mysql:// URL — the adapter factory rejects this; use DSN format")
	}
	if !strings.Contains(string(compose), "BEACON_DATABASE_ADAPTER") {
		t.Error("docker-compose.yml missing BEACON_DATABASE_ADAPTER for MySQL")
	}
	if !strings.Contains(string(compose), "@tcp(") {
		t.Error("docker-compose.yml missing Go MySQL DSN tcp() syntax")
	}
}

func TestInitSQLite(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	var out, errb bytes.Buffer
	code := cmdInit([]string{"-d", "sqlite"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errb.String())
	}

	compose, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "BEACON_DATABASE_ADAPTER") {
		t.Error("docker-compose.yml missing BEACON_DATABASE_ADAPTER for SQLite")
	}
}

// chdir changes the working directory for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(prev) })
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file %s to exist: %v", path, err)
	}
}

func assertFileNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("expected file %s to NOT exist", path)
	}
}
