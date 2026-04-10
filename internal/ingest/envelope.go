package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

const (
	// MaxNameLen is the per-spec ceiling on event.name.
	MaxNameLen = 128
	// MaxActorTypeLen is the per-spec ceiling on actor_type.
	MaxActorTypeLen = 64
	// MaxPropertiesBytes is the per-spec ceiling on a single event's properties (serialized).
	MaxPropertiesBytes = 16 * 1024

	// ClockSkewFutureTolerance — clients more than this far in the future have
	// their created_at rewritten to server now(). Keeps a broken client clock
	// from poisoning rollups.
	ClockSkewFutureTolerance = 5 * time.Minute
	// LateArrivingThreshold — events older than this are accepted but flagged.
	LateArrivingThreshold = 24 * time.Hour
)

// batchRequest is the POST /events body shape.
type batchRequest struct {
	Events []envelopeJSON `json:"events"`
}

// envelopeJSON is the on-the-wire shape of a single event.
type envelopeJSON struct {
	Kind       string          `json:"kind"`
	Name       string          `json:"name"`
	CreatedAt  string          `json:"created_at"`
	ActorType  string          `json:"actor_type"`
	ActorID    json.RawMessage `json:"actor_id"`
	Properties map[string]any  `json:"properties"`
	Context    map[string]any  `json:"context"`
}

// toEvent validates the envelope and lifts kind-specific fields from
// properties to the top-level beacondb.Event. `now` is injected so tests can
// exercise clock-skew rewrites deterministically.
//
// Returned warnings are non-fatal observations (clock_skew_future_rewritten,
// late_arriving) that the handler logs but does not propagate to the client.
func (e *envelopeJSON) toEvent(now time.Time) (beacondb.Event, []string, error) {
	var warnings []string

	kind := beacondb.Kind(e.Kind)
	switch kind {
	case beacondb.KindOutcome, beacondb.KindPerf, beacondb.KindError:
	default:
		return beacondb.Event{}, nil, fmt.Errorf("kind must be outcome, perf, or error (got %q)", e.Kind)
	}

	if e.Name == "" {
		return beacondb.Event{}, nil, errors.New("name is required")
	}
	if len(e.Name) > MaxNameLen {
		return beacondb.Event{}, nil, fmt.Errorf("name exceeds %d chars", MaxNameLen)
	}
	if len(e.ActorType) > MaxActorTypeLen {
		return beacondb.Event{}, nil, fmt.Errorf("actor_type exceeds %d chars", MaxActorTypeLen)
	}

	if e.CreatedAt == "" {
		return beacondb.Event{}, nil, errors.New("created_at is required")
	}
	clientTime, err := parseRFC3339(e.CreatedAt)
	if err != nil {
		return beacondb.Event{}, nil, fmt.Errorf("created_at must be RFC3339: %w", err)
	}
	clientTime = clientTime.UTC()
	if clientTime.After(now.Add(ClockSkewFutureTolerance)) {
		clientTime = now.UTC()
		warnings = append(warnings, "clock_skew_future_rewritten")
	} else if clientTime.Before(now.Add(-LateArrivingThreshold)) {
		warnings = append(warnings, "late_arriving")
	}

	var actorID int64
	if len(e.ActorID) > 0 {
		id, perr := parseActorID(e.ActorID)
		if perr != nil {
			return beacondb.Event{}, nil, fmt.Errorf("actor_id: %w", perr)
		}
		actorID = id
	}

	// Properties size ceiling. We also use this same encoding later for the
	// event row, so the cost is paid once.
	if e.Properties != nil {
		serialized, err := json.Marshal(e.Properties)
		if err != nil {
			return beacondb.Event{}, nil, fmt.Errorf("properties not JSON-encodable: %w", err)
		}
		if len(serialized) > MaxPropertiesBytes {
			return beacondb.Event{}, nil, fmt.Errorf("properties exceed %d bytes", MaxPropertiesBytes)
		}
	}

	out := beacondb.Event{
		Kind:      kind,
		Name:      e.Name,
		ActorType: e.ActorType,
		ActorID:   actorID,
		Context:   e.Context,
		CreatedAt: clientTime,
	}

	// Copy properties so we can delete kind-specific keys without mutating
	// the decoded envelope.
	var props map[string]any
	if e.Properties != nil {
		props = make(map[string]any, len(e.Properties))
		for k, v := range e.Properties {
			props[k] = v
		}
	}

	switch kind {
	case beacondb.KindPerf:
		dur, ok, derr := extractInt32(props, "duration_ms")
		if derr != nil {
			return beacondb.Event{}, nil, fmt.Errorf("properties.duration_ms: %w", derr)
		}
		if !ok {
			return beacondb.Event{}, nil, errors.New("properties.duration_ms is required for perf events")
		}
		out.DurationMs = &dur
		delete(props, "duration_ms")
		if status, ok, serr := extractInt32(props, "status"); serr != nil {
			return beacondb.Event{}, nil, fmt.Errorf("properties.status: %w", serr)
		} else if ok {
			out.Status = &status
			delete(props, "status")
		}
	case beacondb.KindError:
		fp, _ := props["fingerprint"].(string)
		if fp == "" {
			return beacondb.Event{}, nil, errors.New("properties.fingerprint is required for error events")
		}
		out.Fingerprint = fp
		delete(props, "fingerprint")
	}

	if len(props) == 0 {
		props = nil
	}
	out.Properties = props
	return out, warnings, nil
}

func parseRFC3339(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// parseActorID accepts a JSON number or a JSON string containing digits.
// Non-numeric strings are rejected; v1's storage model is BIGINT and we
// don't want to silently drop or hash non-numeric IDs.
func parseActorID(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if len(s) == 0 || s == "null" {
		return 0, nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, err
		}
		if str == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(str, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("expected integer or digit-string, got %q", str)
		}
		return n, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	return n.Int64()
}

// extractInt32 pulls a numeric field from a properties map, accepting the
// Go types a JSON decoder might produce. Values outside the int32 range
// are rejected rather than silently truncated — a duration_ms of 2^31+1
// would otherwise land in the DB as a negative number.
func extractInt32(props map[string]any, key string) (int32, bool, error) {
	if props == nil {
		return 0, false, nil
	}
	v, ok := props[key]
	if !ok {
		return 0, false, nil
	}
	var n int64
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, true, fmt.Errorf("not a finite number")
		}
		if x != math.Trunc(x) {
			return 0, true, fmt.Errorf("expected integer, got fractional %v", x)
		}
		n = int64(x)
	case int:
		n = int64(x)
	case int32:
		return x, true, nil
	case int64:
		n = x
	case json.Number:
		m, err := x.Int64()
		if err != nil {
			return 0, true, err
		}
		n = m
	default:
		return 0, true, fmt.Errorf("expected number, got %T", v)
	}
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, true, fmt.Errorf("value %d out of int32 range", n)
	}
	return int32(n), true, nil
}
