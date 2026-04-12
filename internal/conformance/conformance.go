// Package conformance holds reference implementations of the cross-client
// algorithms defined in .doc/definition/06-http-api.md (Fingerprint, path
// normalization) and the loader for spec/fixtures.json.
//
// Every language client ships the same algorithms and asserts them against
// spec/fixtures.json in its CI. This package is the Go side of that loop:
// the reference implementations live here and the test in this package
// loads the shared fixture file and asserts each case matches.
//
// If a client disagrees with a fixture, the client is wrong. If the Go
// reference here disagrees with a fixture, either the fixture or the Go
// reference is wrong — never silently diverge.
package conformance

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// Fingerprint computes a Beacon error fingerprint per the normative
// algorithm in 06-http-api.md:
//
//	input  = "<exception_class>|<first_app_frame_path>"
//	hash   = SHA1(input.utf8_bytes)
//	output = lowercase_hex(hash)
//
// firstAppFrame is the client's display form ("relative/path.rb:LINE").
// The line-number suffix is stripped before hashing — cosmetic edits above
// the failing line must not shatter grouping across deploys. A frame that
// carries no line number is used as-is.
func Fingerprint(exceptionClass, firstAppFrame string) string {
	path := stripLineNumber(firstAppFrame)
	h := sha1.New()
	h.Write([]byte(exceptionClass + "|" + path))
	return hex.EncodeToString(h.Sum(nil))
}

func stripLineNumber(frame string) string {
	i := strings.LastIndex(frame, ":")
	if i <= 0 {
		return frame
	}
	// Everything after the last colon must be a valid integer for us to
	// treat it as a line number. This keeps Windows drive-letter paths and
	// URL-looking frames from getting truncated accidentally.
	if _, err := strconv.Atoi(frame[i+1:]); err != nil {
		return frame
	}
	return frame[:i]
}

// ---------------------------------------------------------------------------
// Path normalization
// ---------------------------------------------------------------------------

var (
	numericSegment = regexp.MustCompile(`^\d+$`)
	uuidSegment    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	tokenSegment   = regexp.MustCompile(`^[A-Za-z0-9_-]{22,}$`)
)

// NormalizePath applies the fallback heuristic in 06-http-api.md to produce
// a normalized perf metric name of the form "<METHOD> <normalized_path>".
//
// Clients should prefer their framework's route template (Rails routes,
// Express req.route.path, FastAPI route.path) when one is available. This
// fallback runs when no template is exposed.
//
// Rules, applied left-to-right per segment:
//
//  1. ^\d+$             → :id
//  2. canonical UUID    → :uuid
//  3. ^[A-Za-z0-9_-]{22,}$ → :token   (base64 / JWT-ish opaque IDs)
//  4. otherwise         → preserved as-is
//
// A trailing slash on the input path is preserved. Query strings are
// stripped. The method is uppercased.
func NormalizePath(method, rawPath string) string {
	method = strings.ToUpper(strings.TrimSpace(method))

	if i := strings.IndexByte(rawPath, '?'); i >= 0 {
		rawPath = rawPath[:i]
	}

	hadTrailingSlash := len(rawPath) > 1 && strings.HasSuffix(rawPath, "/")
	segments := strings.Split(strings.TrimSuffix(rawPath, "/"), "/")
	for i, seg := range segments {
		if i == 0 && seg == "" {
			continue // leading slash keeps an empty first segment
		}
		segments[i] = normalizeSegment(seg)
	}
	out := strings.Join(segments, "/")
	if hadTrailingSlash {
		out += "/"
	}
	if out == "" {
		out = "/"
	}
	return method + " " + out
}

func normalizeSegment(seg string) string {
	switch {
	case numericSegment.MatchString(seg):
		return ":id"
	case uuidSegment.MatchString(seg):
		return ":uuid"
	case tokenSegment.MatchString(seg):
		return ":token"
	default:
		return seg
	}
}
