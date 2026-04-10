package beacondb

import (
	"strings"
	"testing"
)

func TestCanonicalJSONKeyOrderIndependence(t *testing.T) {
	a := map[string]any{"plan": "pro", "country": "FR"}
	b := map[string]any{"country": "FR", "plan": "pro"}

	ra, err := CanonicalJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := CanonicalJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ra) != string(rb) {
		t.Errorf("canonical output differs by insertion order:\n a=%s\n b=%s", ra, rb)
	}
	want := `{"country":"FR","plan":"pro"}`
	if string(ra) != want {
		t.Errorf("canonical output = %s, want %s", ra, want)
	}
}

func TestCanonicalJSONNested(t *testing.T) {
	v := map[string]any{
		"b": map[string]any{"z": 1, "a": 2},
		"a": []any{3, "x", true, nil},
	}
	got, err := CanonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":[3,"x",true,null],"b":{"a":2,"z":1}}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCanonicalJSONUnsupportedType(t *testing.T) {
	v := map[string]any{"bad": make(chan int)}
	_, err := CanonicalJSON(v)
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDimensionHashEmpty(t *testing.T) {
	for _, dims := range []map[string]any{nil, {}} {
		h, err := DimensionHash(dims)
		if err != nil {
			t.Fatal(err)
		}
		if h != "" {
			t.Errorf("empty dims hash = %q, want sentinel \"\"", h)
		}
	}
}

func TestDimensionHashStable(t *testing.T) {
	a := map[string]any{"plan": "pro", "country": "FR"}
	b := map[string]any{"country": "FR", "plan": "pro"}
	ha, err := DimensionHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := DimensionHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("hashes differ: %s vs %s", ha, hb)
	}
	if len(ha) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(ha))
	}
}

func TestDimensionHashDistinct(t *testing.T) {
	a := map[string]any{"plan": "pro"}
	b := map[string]any{"plan": "free"}
	ha, _ := DimensionHash(a)
	hb, _ := DimensionHash(b)
	if ha == hb {
		t.Error("different dimensions hashed to the same value")
	}
}

func TestKindValid(t *testing.T) {
	for _, k := range []Kind{KindOutcome, KindPerf, KindError, KindBaseline} {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range []Kind{"", "banana", "ERROR"} {
		if Kind(k).Valid() {
			t.Errorf("%q should be invalid", k)
		}
	}
}
