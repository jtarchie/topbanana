package server

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v5"
)

// TestNotFoundHelperShape guards the slog-echo interaction: the sentinel
// echo.ErrNotFound is an unexported *httpError, and slog-echo's
// `err.(*echo.HTTPError)` type-assertion fails on it, after which slog-echo
// wraps it in a fresh 500 — that's why we use notFound() instead of
// echo.ErrNotFound. If this test breaks, every 404 in the codebase will
// silently start returning 500 again.
func TestNotFoundHelperShape(t *testing.T) {
	err := notFound()

	// Must be a concrete *echo.HTTPError, not the unexported sentinel type.
	var asHTTP *echo.HTTPError
	asHTTP, ok := any(err).(*echo.HTTPError)
	if !ok {
		t.Fatalf("notFound() must return *echo.HTTPError so slog-echo doesn't wrap it as 500; got %T", err)
	}
	if asHTTP.Code != http.StatusNotFound {
		t.Fatalf("code: got %d, want %d", asHTTP.Code, http.StatusNotFound)
	}
}
