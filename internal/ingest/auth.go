package ingest

import "github.com/luuuc/beacon/internal/httputil"

// checkBearer is a thin forwarder to the shared httputil helper so the
// ingest tests keep their existing call shape.
func checkBearer(authHeader, expected string) bool {
	return httputil.CheckBearer(authHeader, expected)
}
