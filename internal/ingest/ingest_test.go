package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/beacondb/memfake"
)

// fixedNow is the reference clock used by every envelope test so clock-skew
// behavior is deterministic.
var fixedNow = time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

func newTestHandler(t *testing.T, cfg Config) (*Handler, *memfake.Fake) {
	t.Helper()
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatalf("fake.Migrate: %v", err)
	}
	h := NewHandler(cfg, fake, nil)
	h.now = func() time.Time { return fixedNow }
	return h, fake
}

func makeBatch(events ...map[string]any) *bytes.Buffer {
	body, _ := json.Marshal(map[string]any{"events": events})
	return bytes.NewBuffer(body)
}

func doPost(t *testing.T, h *Handler, body *bytes.Buffer, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/events", body)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:12345"
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Response matrix
// ---------------------------------------------------------------------------

func TestAccepted202(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	body := makeBatch(
		map[string]any{
			"kind":       "outcome",
			"name":       "signup.completed",
			"created_at": fixedNow.Format(time.RFC3339),
			"properties": map[string]any{"plan": "pro"},
			"context":    map[string]any{"request_id": "abc"},
		},
		map[string]any{
			"kind":       "perf",
			"name":       "GET /",
			"created_at": fixedNow.Format(time.RFC3339),
			"properties": map[string]any{"duration_ms": 187, "status": 200},
		},
		map[string]any{
			"kind":       "error",
			"name":       "NoMethodError",
			"created_at": fixedNow.Format(time.RFC3339),
			"properties": map[string]any{"fingerprint": "abc123", "message": "boom"},
		},
		map[string]any{
			"kind":       "ambient",
			"name":       "http_request",
			"created_at": fixedNow.Format(time.RFC3339),
			"properties": map[string]any{"path": "/search", "method": "GET", "status": 200},
			"dimensions": map[string]any{"country": "US", "plan": "pro"},
		},
	)
	rec := doPost(t, h, body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	mustDecode(t, rec.Body.Bytes(), &resp)
	if resp["received"].(float64) != 4 {
		t.Errorf("received = %v, want 4", resp["received"])
	}

	// Verify the fake actually got the rows and the kind-specific lifts.
	ctx := context.Background()
	outcomes, _ := fake.ListEvents(ctx, beacondb.EventFilter{Kind: beacondb.KindOutcome})
	if len(outcomes) != 1 || outcomes[0].Name != "signup.completed" {
		t.Errorf("outcomes = %+v", outcomes)
	}
	perfs, _ := fake.ListEvents(ctx, beacondb.EventFilter{Kind: beacondb.KindPerf})
	if len(perfs) != 1 {
		t.Fatalf("perfs = %+v", perfs)
	}
	if perfs[0].DurationMs == nil || *perfs[0].DurationMs != 187 {
		t.Errorf("duration_ms not lifted: %+v", perfs[0])
	}
	if _, leftover := perfs[0].Properties["duration_ms"]; leftover {
		t.Errorf("duration_ms should be removed from properties: %+v", perfs[0].Properties)
	}
	errs, _ := fake.ListEvents(ctx, beacondb.EventFilter{Kind: beacondb.KindError})
	if len(errs) != 1 || errs[0].Fingerprint != "abc123" {
		t.Errorf("errors fingerprint not lifted: %+v", errs)
	}
	ambients, _ := fake.ListEvents(ctx, beacondb.EventFilter{Kind: beacondb.KindAmbient})
	if len(ambients) != 1 {
		t.Fatalf("ambients = %+v", ambients)
	}
	if ambients[0].Name != "http_request" {
		t.Errorf("ambient name = %q, want http_request", ambients[0].Name)
	}
	// Ambient events keep all properties intact (no field lifting).
	if ambients[0].Properties["path"] != "/search" {
		t.Errorf("ambient properties.path not preserved: %+v", ambients[0].Properties)
	}
	// Dimensions are stored separately from properties.
	if ambients[0].Dimensions["country"] != "US" || ambients[0].Dimensions["plan"] != "pro" {
		t.Errorf("ambient dimensions not preserved: %+v", ambients[0].Dimensions)
	}
}

func TestBadRequest400(t *testing.T) {
	cases := []struct {
		name     string
		events   []map[string]any
		wantMsg  string
	}{
		{
			"empty batch",
			nil,
			"events array is empty",
		},
		{
			"unknown kind",
			[]map[string]any{{"kind": "banana", "name": "x", "created_at": fixedNow.Format(time.RFC3339)}},
			"kind must be outcome, perf, error, or ambient",
		},
		{
			"missing name",
			[]map[string]any{{"kind": "outcome", "created_at": fixedNow.Format(time.RFC3339)}},
			"name is required",
		},
		{
			"name too long",
			[]map[string]any{{"kind": "outcome", "name": strings.Repeat("x", 129), "created_at": fixedNow.Format(time.RFC3339)}},
			"name exceeds",
		},
		{
			"missing created_at",
			[]map[string]any{{"kind": "outcome", "name": "x"}},
			"created_at is required",
		},
		{
			"bad created_at",
			[]map[string]any{{"kind": "outcome", "name": "x", "created_at": "yesterday"}},
			"created_at must be RFC3339",
		},
		{
			"perf missing duration_ms",
			[]map[string]any{{"kind": "perf", "name": "GET /", "created_at": fixedNow.Format(time.RFC3339)}},
			"duration_ms is required",
		},
		{
			"error missing fingerprint",
			[]map[string]any{{"kind": "error", "name": "Boom", "created_at": fixedNow.Format(time.RFC3339), "properties": map[string]any{}}},
			"fingerprint is required",
		},
		{
			"actor_id exceeds max length",
			[]map[string]any{{
				"kind":       "outcome",
				"name":       "x",
				"created_at": fixedNow.Format(time.RFC3339),
				"actor_id":   strings.Repeat("a", 129),
			}},
			"actor_id exceeds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t, Config{})
			body := makeBatch(tc.events...)
			rec := doPost(t, h, body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d (%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantMsg) {
				t.Errorf("body missing %q: %s", tc.wantMsg, rec.Body.String())
			}
			// 400 bodies must carry events_rejected.
			var resp map[string]any
			mustDecode(t, rec.Body.Bytes(), &resp)
			if _, ok := resp["events_rejected"]; !ok {
				t.Errorf("400 body missing events_rejected: %v", resp)
			}
		})
	}
}

// TestActorIDAcceptsAnyString pins the v0.2.0 contract: actor_id is now
// a free-form string up to MaxActorIDLen. UUIDs (Rails 7.1+), ULIDs,
// Snowflakes, legacy integers, and integer-as-string all round-trip
// losslessly to the stored Event.
func TestActorIDAcceptsAnyString(t *testing.T) {
	cases := []struct {
		name    string
		rawJSON string
		want    string
	}{
		{"uuid v7", `"019245ab-d36e-7000-8000-000000000001"`, "019245ab-d36e-7000-8000-000000000001"},
		{"ulid", `"01HZABCDEFGHJKMNPQRSTVWXYZ"`, "01HZABCDEFGHJKMNPQRSTVWXYZ"},
		{"integer as number", `42`, "42"},
		{"integer as string", `"42"`, "42"},
		{"string prefixed", `"acct_x1Y2z3A4b5C6"`, "acct_x1Y2z3A4b5C6"},
		{"bigint beyond int64", `"99999999999999999999"`, "99999999999999999999"},
		{"empty string", `""`, ""},
		{"null", `null`, ""},
		{"field omitted", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, fake := newTestHandler(t, Config{})
			event := map[string]any{
				"kind":       "outcome",
				"name":       "actor.test",
				"created_at": fixedNow.Format(time.RFC3339),
				"actor_type": "User",
			}
			if tc.rawJSON != "" {
				var v any
				if err := json.Unmarshal([]byte(tc.rawJSON), &v); err != nil {
					t.Fatalf("unmarshal raw: %v", err)
				}
				event["actor_id"] = v
			}
			rec := doPost(t, h, makeBatch(event), nil)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("code = %d (%s)", rec.Code, rec.Body.String())
			}
			events, err := fake.ListEvents(context.Background(), beacondb.EventFilter{Name: "actor.test"})
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("len(events) = %d, want 1", len(events))
			}
			if got := events[0].ActorID; got != tc.want {
				t.Errorf("ActorID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnauthorized401(t *testing.T) {
	h, _ := newTestHandler(t, Config{AuthToken: "secret"})
	body := makeBatch(map[string]any{
		"kind":       "outcome",
		"name":       "x",
		"created_at": fixedNow.Format(time.RFC3339),
	})

	t.Run("missing header", func(t *testing.T) {
		rec := doPost(t, h, body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("code = %d", rec.Code)
		}
	})
	t.Run("wrong token", func(t *testing.T) {
		rec := doPost(t, h, makeBatch(map[string]any{
			"kind": "outcome", "name": "x", "created_at": fixedNow.Format(time.RFC3339),
		}), func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer wrong")
		})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("code = %d", rec.Code)
		}
	})
	t.Run("correct token", func(t *testing.T) {
		rec := doPost(t, h, makeBatch(map[string]any{
			"kind": "outcome", "name": "x", "created_at": fixedNow.Format(time.RFC3339),
		}), func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer secret")
		})
		if rec.Code != http.StatusAccepted {
			t.Errorf("code = %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestPayloadTooLarge413(t *testing.T) {
	t.Run("too many events", func(t *testing.T) {
		h, _ := newTestHandler(t, Config{MaxEventsPerBatch: 2})
		body := makeBatch(
			map[string]any{"kind": "outcome", "name": "a", "created_at": fixedNow.Format(time.RFC3339)},
			map[string]any{"kind": "outcome", "name": "b", "created_at": fixedNow.Format(time.RFC3339)},
			map[string]any{"kind": "outcome", "name": "c", "created_at": fixedNow.Format(time.RFC3339)},
		)
		rec := doPost(t, h, body, nil)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("code = %d", rec.Code)
		}
	})
	t.Run("body too large", func(t *testing.T) {
		h, _ := newTestHandler(t, Config{MaxBodyBytes: 256})
		// Build a body > 256 bytes.
		big := strings.Repeat("x", 500)
		body := makeBatch(map[string]any{
			"kind":       "outcome",
			"name":       "x",
			"created_at": fixedNow.Format(time.RFC3339),
			"properties": map[string]any{"blob": big},
		})
		rec := doPost(t, h, body, nil)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("code = %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestRateLimit429(t *testing.T) {
	h, _ := newTestHandler(t, Config{RatePerSecond: 2})
	body := func() *bytes.Buffer {
		return makeBatch(map[string]any{
			"kind": "outcome", "name": "x", "created_at": fixedNow.Format(time.RFC3339),
		})
	}
	// The bucket is initialized with burst = rate = 2. First two go through,
	// third should 429 with Retry-After.
	for i := 0; i < 2; i++ {
		rec := doPost(t, h, body(), nil)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("req %d: code = %d", i, rec.Code)
		}
	}
	rec := doPost(t, h, body(), nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("Retry-After header missing on 429")
	}
}

// ---------------------------------------------------------------------------
// Feature tests: idempotency + clock skew
// ---------------------------------------------------------------------------

func TestIdempotencyReplay(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	body := func() *bytes.Buffer {
		return makeBatch(map[string]any{
			"kind": "outcome", "name": "x", "created_at": fixedNow.Format(time.RFC3339),
		})
	}
	setKey := func(r *http.Request) { r.Header.Set("Idempotency-Key", "key-42") }

	rec := doPost(t, h, body(), setKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first: %d", rec.Code)
	}
	rec = doPost(t, h, body(), setKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("replay: %d", rec.Code)
	}
	var resp map[string]any
	mustDecode(t, rec.Body.Bytes(), &resp)
	if dup, _ := resp["duplicate"].(bool); !dup {
		t.Errorf("replay body = %v, expected duplicate:true", resp)
	}
	// Only one row should be in the store.
	events, _ := fake.ListEvents(context.Background(), beacondb.EventFilter{Kind: beacondb.KindOutcome})
	if len(events) != 1 {
		t.Errorf("events written = %d, want 1", len(events))
	}
}

func TestIdempotencyNotRecordedOnFailure(t *testing.T) {
	h, _ := newTestHandler(t, Config{})
	// First attempt: bad batch (no events) — should 400.
	badBody := makeBatch()
	rec := doPost(t, h, badBody, func(r *http.Request) {
		r.Header.Set("Idempotency-Key", "retry-me")
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("first = %d", rec.Code)
	}
	// Retry with the same key and a good batch — should succeed, not 202-duplicate.
	goodBody := makeBatch(map[string]any{
		"kind": "outcome", "name": "x", "created_at": fixedNow.Format(time.RFC3339),
	})
	rec = doPost(t, h, goodBody, func(r *http.Request) {
		r.Header.Set("Idempotency-Key", "retry-me")
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	mustDecode(t, rec.Body.Bytes(), &resp)
	if dup, _ := resp["duplicate"].(bool); dup {
		t.Errorf("retry should not be marked duplicate: %v", resp)
	}
}

// failingInsertAdapter wraps memfake but forces InsertEvents to fail the
// first N times, so the handler returns 503 — the storage-failure path
// the real runbook cares about.
type failingInsertAdapter struct {
	*memfake.Fake
	failures int
}

func (f *failingInsertAdapter) InsertEvents(ctx context.Context, events []beacondb.Event) ([]int64, error) {
	if f.failures > 0 {
		f.failures--
		return nil, fmt.Errorf("simulated storage failure")
	}
	return f.Fake.InsertEvents(ctx, events)
}

func TestIdempotencyNotRecordedOnStorageFailure(t *testing.T) {
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	adapter := &failingInsertAdapter{Fake: fake, failures: 1}
	h := NewHandler(Config{}, adapter, nil)
	h.now = func() time.Time { return fixedNow }

	body := func() *bytes.Buffer {
		return makeBatch(map[string]any{
			"kind": "outcome", "name": "x", "created_at": fixedNow.Format(time.RFC3339),
		})
	}
	setKey := func(r *http.Request) { r.Header.Set("Idempotency-Key", "boom") }

	// First attempt fails at storage — 503, key must NOT be recorded.
	rec := doPost(t, h, body(), setKey)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first = %d (%s)", rec.Code, rec.Body.String())
	}
	// Retry with the same key — must land normally (202, not duplicate).
	rec = doPost(t, h, body(), setKey)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	mustDecode(t, rec.Body.Bytes(), &resp)
	if dup, _ := resp["duplicate"].(bool); dup {
		t.Errorf("retry after storage failure must not be marked duplicate: %v", resp)
	}
	events, _ := fake.ListEvents(context.Background(), beacondb.EventFilter{})
	if len(events) != 1 {
		t.Errorf("expected exactly 1 stored event after retry, got %d", len(events))
	}
}

func TestClockSkewRewriteFuture(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	future := fixedNow.Add(2 * time.Hour).Format(time.RFC3339)
	body := makeBatch(map[string]any{
		"kind": "outcome", "name": "x", "created_at": future,
	})
	rec := doPost(t, h, body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	events, _ := fake.ListEvents(context.Background(), beacondb.EventFilter{Kind: beacondb.KindOutcome})
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if !events[0].CreatedAt.Equal(fixedNow) {
		t.Errorf("created_at = %s, want rewritten to %s", events[0].CreatedAt, fixedNow)
	}
}

func TestClockSkewFutureSmallNotRewritten(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	near := fixedNow.Add(30 * time.Second).Format(time.RFC3339)
	body := makeBatch(map[string]any{
		"kind": "outcome", "name": "x", "created_at": near,
	})
	rec := doPost(t, h, body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	events, _ := fake.ListEvents(context.Background(), beacondb.EventFilter{Kind: beacondb.KindOutcome})
	if events[0].CreatedAt.Equal(fixedNow) {
		t.Error("30s in the future should not have been rewritten")
	}
}

func TestLateArrivingAccepted(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	oldTime := fixedNow.Add(-48 * time.Hour).Format(time.RFC3339)
	body := makeBatch(map[string]any{
		"kind": "outcome", "name": "x", "created_at": oldTime,
	})
	rec := doPost(t, h, body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d (%s)", rec.Code, rec.Body.String())
	}
	events, _ := fake.ListEvents(context.Background(), beacondb.EventFilter{Kind: beacondb.KindOutcome})
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	want := fixedNow.Add(-48 * time.Hour)
	if !events[0].CreatedAt.Equal(want) {
		t.Errorf("late-arriving created_at should be kept as-is, got %s", events[0].CreatedAt)
	}
}

// ---------------------------------------------------------------------------
// Unit tests for the helpers
// ---------------------------------------------------------------------------

func TestClientIP(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*http.Request)
		remote   string
		trustXFF bool
		want     string
	}{
		{"remoteaddr host:port", nil, "192.0.2.5:42", false, "192.0.2.5"},
		{"xff ignored by default", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.9") }, "10.0.0.1:1", false, "10.0.0.1"},
		{"xff trusted when enabled", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.9") }, "10.0.0.1:1", true, "203.0.113.9"},
		{"xff chain first hop", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1") }, "10.0.0.1:1", true, "203.0.113.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/events", nil)
			req.RemoteAddr = tc.remote
			if tc.mutate != nil {
				tc.mutate(req)
			}
			if got := clientIP(req, tc.trustXFF); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBearerCheckConstantTime(t *testing.T) {
	if !checkBearer("Bearer hello", "hello") {
		t.Error("exact match should pass")
	}
	if checkBearer("Bearer helloo", "hello") {
		t.Error("suffix mismatch should fail")
	}
	if checkBearer("Token hello", "hello") {
		t.Error("wrong scheme should fail")
	}
	if checkBearer("", "hello") {
		t.Error("empty header should fail")
	}
}

func mustDecode(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %q: %v", string(body), err)
	}
}

// Silence unused-import lint on fmt when all users live behind t.Run names.
var _ = fmt.Sprintf
