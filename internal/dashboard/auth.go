package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// signingKey is generated once at boot and lives in memory. Restarts
// invalidate all sessions — acceptable for a single-tenant tool.
var (
	signingKeyOnce sync.Once
	signingKey     []byte
)

func getSigningKey() []byte {
	signingKeyOnce.Do(func() {
		signingKey = make([]byte, 32)
		if _, err := rand.Read(signingKey); err != nil {
			panic("dashboard: failed to generate signing key: " + err.Error())
		}
	})
	return signingKey
}

const (
	sessionCookieName = "beacon_session"
	csrfCookieName    = "beacon_csrf"
	csrfFieldName     = "csrf_token"
	sessionValue      = "authenticated"
	cookieMaxAge      = 7 * 24 * 60 * 60 // 7 days
)

// signValue produces an HMAC-SHA256 hex signature for the given value.
func signValue(value string) string {
	mac := hmac.New(sha256.New, getSigningKey())
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// signedCookieValue returns "value.signature".
func signedCookieValue(value string) string {
	return value + "." + signValue(value)
}

// verifySignedCookie checks that the cookie value matches "value.signature".
func verifySignedCookie(raw string) (string, bool) {
	dot := -1
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		return "", false
	}
	value := raw[:dot]
	sig := raw[dot+1:]
	expected := signValue(value)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return "", false
	}
	return value, true
}

// authMiddleware returns middleware that gates dashboard routes behind
// BEACON_AUTH_TOKEN. When the token is empty, all requests pass through.
func (d *Dashboard) authMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d.cfg.AuthToken == "" {
				next.ServeHTTP(w, r)
				return
			}
			cookie, err := r.Cookie(sessionCookieName)
			if err != nil {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			value, ok := verifySignedCookie(cookie.Value)
			if !ok || value != sessionValue {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (d *Dashboard) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if d.cfg.AuthToken == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	csrf := generateCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    csrf,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   600, // 10 minutes
	})
	d.render(w, r, "login.html", "", map[string]any{
		"CSRFToken": csrf,
		"Error":     r.URL.Query().Get("error"),
	})
}

func (d *Dashboard) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if d.cfg.AuthToken == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// CSRF check.
	csrfCookie, err := r.Cookie(csrfCookieName)
	if err != nil || csrfCookie.Value == "" {
		http.Redirect(w, r, "/login?error=invalid+request", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?error=invalid+request", http.StatusFound)
		return
	}
	formCSRF := r.FormValue(csrfFieldName)
	if subtle.ConstantTimeCompare([]byte(csrfCookie.Value), []byte(formCSRF)) != 1 {
		http.Redirect(w, r, "/login?error=invalid+request", http.StatusFound)
		return
	}

	// Token check.
	token := r.FormValue("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(d.cfg.AuthToken)) != 1 {
		http.Redirect(w, r, "/login?error=invalid+token", http.StatusFound)
		return
	}

	// Set session cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    signedCookieValue(sessionValue),
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   cookieMaxAge,
	})
	// Clear CSRF cookie.
	http.SetCookie(w, &http.Cookie{
		Name:   csrfCookieName,
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func generateCSRFToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("dashboard: csrf rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
