package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixturesPath walks up from this test file to the repo root and appends
// spec/fixtures.json. That keeps the test independent of where `go test`
// is invoked from.
func fixturesPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile) // .../internal/conformance
	repoRoot := filepath.Join(dir, "..", "..")
	p := filepath.Join(repoRoot, "spec", "fixtures.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixtures not found at %s: %v", p, err)
	}
	return p
}

type fixtures struct {
	Fingerprint struct {
		Cases []struct {
			Name                string `json:"name"`
			ExceptionClass      string `json:"exception_class"`
			FirstAppFrame       string `json:"first_app_frame"`
			ExpectedFingerprint string `json:"expected_fingerprint"`
		} `json:"cases"`
	} `json:"fingerprint"`

	PathNormalization struct {
		Cases []struct {
			Name         string `json:"name"`
			Method       string `json:"method"`
			RawPath      string `json:"raw_path"`
			ExpectedName string `json:"expected_name"`
		} `json:"cases"`
	} `json:"path_normalization"`

	Envelope struct {
		Cases []struct {
			Name                  string         `json:"name"`
			Valid                 bool           `json:"valid"`
			Event                 map[string]any `json:"event"`
			ExpectedErrorContains string         `json:"expected_error_contains"`
		} `json:"cases"`
	} `json:"envelope"`

	BatchFlush        json.RawMessage `json:"batch_flush"`
	Unreachable       json.RawMessage `json:"unreachable_beacon"`
	DeploySHAPropagation json.RawMessage `json:"deploy_sha_propagation"`
}

func loadFixtures(t *testing.T) fixtures {
	t.Helper()
	raw, err := os.ReadFile(fixturesPath(t))
	if err != nil {
		t.Fatal(err)
	}
	var f fixtures
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixtures.json: %v", err)
	}
	return f
}

// TestFingerprintFixtures runs every fingerprint case from spec/fixtures.json
// through the Go reference implementation and asserts the expected hex. This
// is the byte-for-byte cross-client guarantee.
func TestFingerprintFixtures(t *testing.T) {
	f := loadFixtures(t)
	if len(f.Fingerprint.Cases) == 0 {
		t.Fatal("fingerprint section is empty")
	}
	for _, c := range f.Fingerprint.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := Fingerprint(c.ExceptionClass, c.FirstAppFrame)
			if got != c.ExpectedFingerprint {
				t.Errorf("Fingerprint(%q, %q)\n  got  %s\n  want %s",
					c.ExceptionClass, c.FirstAppFrame, got, c.ExpectedFingerprint)
			}
		})
	}
}

// TestFingerprintSameFileDifferentLine is the loudly-named guarantee from the
// fingerprint algorithm doc: cosmetic edits above the failing line must not
// change the fingerprint.
func TestFingerprintSameFileDifferentLine(t *testing.T) {
	a := Fingerprint("NoMethodError", "app/foo.rb:10")
	b := Fingerprint("NoMethodError", "app/foo.rb:200")
	if a != b {
		t.Errorf("line number leaked into fingerprint:\n  line 10:  %s\n  line 200: %s", a, b)
	}
}

// TestPathNormalizationFixtures runs every path normalization case from
// spec/fixtures.json through the Go reference implementation.
func TestPathNormalizationFixtures(t *testing.T) {
	f := loadFixtures(t)
	if len(f.PathNormalization.Cases) == 0 {
		t.Fatal("path_normalization section is empty")
	}
	for _, c := range f.PathNormalization.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := NormalizePath(c.Method, c.RawPath)
			if got != c.ExpectedName {
				t.Errorf("NormalizePath(%q, %q)\n  got  %q\n  want %q",
					c.Method, c.RawPath, got, c.ExpectedName)
			}
		})
	}
}

// TestEnvelopeFixturesShape asserts the envelope section parses and has the
// expected cases. Full server-side validation happens in the ingest package's
// own tests; this just makes sure the fixture file is well-formed and the
// Ruby client has fresh material to load from.
func TestEnvelopeFixturesShape(t *testing.T) {
	f := loadFixtures(t)
	if len(f.Envelope.Cases) == 0 {
		t.Fatal("envelope section is empty")
	}
	var validCount, invalidCount int
	for _, c := range f.Envelope.Cases {
		if c.Event == nil {
			t.Errorf("%s: missing event map", c.Name)
		}
		if c.Valid {
			validCount++
		} else {
			invalidCount++
			if c.ExpectedErrorContains == "" {
				t.Errorf("%s: invalid case missing expected_error_contains", c.Name)
			}
		}
	}
	if validCount < 2 || invalidCount < 2 {
		t.Errorf("envelope cases unbalanced: valid=%d invalid=%d (need at least 2 of each)", validCount, invalidCount)
	}
}

// TestDescriptiveSectionsPresent makes sure the client-only sections
// (batch_flush, unreachable_beacon, deploy_sha_propagation) are present in
// the fixture file so the Ruby client can't load a stale copy.
func TestDescriptiveSectionsPresent(t *testing.T) {
	f := loadFixtures(t)
	if len(f.BatchFlush) < 2 || !strings.Contains(string(f.BatchFlush), "rules") {
		t.Errorf("batch_flush missing or empty: %s", f.BatchFlush)
	}
	if len(f.Unreachable) < 2 || !strings.Contains(string(f.Unreachable), "rules") {
		t.Errorf("unreachable_beacon missing or empty: %s", f.Unreachable)
	}
	if len(f.DeploySHAPropagation) < 2 || !strings.Contains(string(f.DeploySHAPropagation), "cases") {
		t.Errorf("deploy_sha_propagation missing or empty: %s", f.DeploySHAPropagation)
	}
}
