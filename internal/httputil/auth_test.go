package httputil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckBearer(t *testing.T) {
	cases := []struct {
		name   string
		header string
		token  string
		want   bool
	}{
		{"valid", "Bearer secret", "secret", true},
		{"wrong token", "Bearer wrong", "secret", false},
		{"no prefix", "secret", "secret", false},
		{"empty header", "", "secret", false},
		{"basic auth", "Basic dXNlcjpwYXNz", "secret", false},
		{"bearer lowercase", "bearer secret", "secret", false},
		{"empty token after prefix", "Bearer ", "secret", false},
		{"both empty after prefix", "Bearer ", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckBearer(tc.header, tc.token)
			if got != tc.want {
				t.Errorf("CheckBearer(%q, %q) = %v, want %v", tc.header, tc.token, got, tc.want)
			}
		})
	}
}

func TestBearerMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("empty token passes through", func(t *testing.T) {
		h := BearerMiddleware("", inner)
		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("valid token passes", func(t *testing.T) {
		h := BearerMiddleware("tok", inner)
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("missing token rejects", func(t *testing.T) {
		h := BearerMiddleware("tok", inner)
		req := httptest.NewRequest("GET", "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong token rejects", func(t *testing.T) {
		h := BearerMiddleware("tok", inner)
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		var body map[string]string
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["error"] != "invalid token" {
			t.Errorf("error = %q, want 'invalid token'", body["error"])
		}
	})
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]int{"count": 42})
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var body map[string]int
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["count"] != 42 {
		t.Errorf("count = %d, want 42", body["count"])
	}
}

func TestWriteJSONError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSONError(rec, http.StatusBadRequest, "bad input")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "bad input" {
		t.Errorf("error = %q, want 'bad input'", body["error"])
	}
}
