// Package httputil holds cross-handler HTTP helpers. Kept deliberately
// small — anything that belongs to a specific subsystem (ingest, reads)
// stays in that subsystem.
package httputil

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// CheckBearer returns true when authHeader carries the expected token.
// A constant-time compare keeps response time independent of the submitted
// token. An empty expected token is a programming error — callers should
// skip the check when auth is disabled.
func CheckBearer(authHeader, expected string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return false
	}
	got := authHeader[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// BearerMiddleware wraps next with a Bearer-token gate. An empty token
// disables the gate (for loopback-only deployments where the server is
// unreachable from anywhere but the host app).
func BearerMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !CheckBearer(r.Header.Get("Authorization"), token) {
			WriteJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WriteJSON writes body as JSON with the given status code. Errors from
// the encoder are silently dropped — the client can't recover from a mid-
// response failure and the log noise isn't worth it.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteJSONError is the shorthand for an error envelope.
func WriteJSONError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}
