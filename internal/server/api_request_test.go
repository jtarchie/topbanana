package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/sandbox"
	"github.com/jtarchie/topbanana/internal/state"
)

// --- buildSandboxRequest / body parsers --------------------------------------
//
// These turn untrusted public HTTP bodies into the map handed to user-authored
// JS; previously only reachable through full e2e.

func TestParseFormBody(t *testing.T) {
	t.Parallel()

	t.Run("valid form keeps first value per key", func(t *testing.T) {
		t.Parallel()
		form, err := parseFormBody([]byte("a=1&a=2&b=x+y"))
		if err != nil {
			t.Fatal(err)
		}
		if form["a"] != "1" || form["b"] != "x y" {
			t.Errorf("form = %v", form)
		}
	})

	t.Run("malformed body degrades to empty map, no error", func(t *testing.T) {
		t.Parallel()
		form, err := parseFormBody([]byte("%zz=&;;;"))
		if err != nil {
			t.Fatalf("malformed form should not error: %v", err)
		}
		if len(form) != 0 {
			t.Errorf("form = %v, want empty", form)
		}
	})

	t.Run("oversized field maps to 413 sentinel", func(t *testing.T) {
		t.Parallel()
		big := strings.Repeat("a", maxAPIFieldBytes+1)
		_, err := parseFormBody([]byte("bio=" + big))
		if !errors.Is(err, errAPIPayloadTooLarge) {
			t.Fatalf("err = %v, want errAPIPayloadTooLarge", err)
		}
	})
}

func TestParseJSONBody(t *testing.T) {
	t.Parallel()

	t.Run("valid object parses", func(t *testing.T) {
		t.Parallel()
		parsed, err := parseJSONBody([]byte(`{"name":"ada","n":3}`))
		if err != nil {
			t.Fatal(err)
		}
		obj, ok := parsed.(map[string]any)
		if !ok || obj["name"] != "ada" {
			t.Errorf("parsed = %#v", parsed)
		}
	})

	t.Run("invalid JSON degrades to nil, no error", func(t *testing.T) {
		t.Parallel()
		parsed, err := parseJSONBody([]byte(`{not json`))
		if err != nil || parsed != nil {
			t.Errorf("parsed=%v err=%v, want nil/nil", parsed, err)
		}
	})

	t.Run("oversized top-level string maps to 413 sentinel", func(t *testing.T) {
		t.Parallel()
		big := strings.Repeat("a", maxAPIFieldBytes+1)
		_, err := parseJSONBody([]byte(`{"bio":"` + big + `"}`))
		if !errors.Is(err, errAPIPayloadTooLarge) {
			t.Fatalf("err = %v, want errAPIPayloadTooLarge", err)
		}
	})

	t.Run("non-object JSON passes through", func(t *testing.T) {
		t.Parallel()
		parsed, err := parseJSONBody([]byte(`[1,2,3]`))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := parsed.([]any); !ok {
			t.Errorf("parsed = %#v, want array", parsed)
		}
	})
}

func TestBuildSandboxRequest(t *testing.T) {
	t.Parallel()

	t.Run("copies method, path, query, lowercased headers", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest(http.MethodPost, "/api/submit?page=2&page=3", strings.NewReader("x=1"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Custom-Header", "val")
		req, err := buildSandboxRequest(r, "submit")
		if err != nil {
			t.Fatal(err)
		}
		if req.Method != http.MethodPost || req.Path != "/api/submit" {
			t.Errorf("method/path = %s %s", req.Method, req.Path)
		}
		if req.Query["page"] != "2" {
			t.Errorf("query first-value = %v", req.Query)
		}
		if req.Headers["x-custom-header"] != "val" {
			t.Errorf("headers not lowercased: %v", req.Headers)
		}
		if req.Form["x"] != "1" {
			t.Errorf("form not populated: %v", req.Form)
		}
	})

	t.Run("json content type populates request.json", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest(http.MethodPost, "/api/submit", strings.NewReader(`{"k":"v"}`))
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		req, err := buildSandboxRequest(r, "submit")
		if err != nil {
			t.Fatal(err)
		}
		obj, ok := req.JSON.(map[string]any)
		if !ok || obj["k"] != "v" {
			t.Errorf("JSON = %#v", req.JSON)
		}
		if req.Body != `{"k":"v"}` {
			t.Errorf("raw body = %q", req.Body)
		}
	})

	t.Run("over-cap body maps to 413 sentinel", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest(http.MethodPost, "/api/submit", strings.NewReader(strings.Repeat("a", maxAPIBodyBytes+1)))
		_, err := buildSandboxRequest(r, "submit")
		if !errors.Is(err, errAPIPayloadTooLarge) {
			t.Fatalf("err = %v, want errAPIPayloadTooLarge", err)
		}
	})
}

// --- invokeWithCAS ------------------------------------------------------------

// conflictingStore wraps a real state.Store and forces ErrConflict on the
// first N Saves, counting every attempt — the seam for asserting the retry
// loop actually re-runs the handler and eventually surfaces 503.
type conflictingStore struct {
	state.Store
	conflictsLeft int
	saves         int
}

func (c *conflictingStore) Save(ctx context.Context, slug string, snap *state.Snapshot) error {
	c.saves++
	if c.conflictsLeft > 0 {
		c.conflictsLeft--
		return state.ErrConflict
	}
	// Deliberately unwrapped: the stub must hand invokeWithCAS exactly the
	// error a production store would, or the test isn't testing the real path.
	return c.Store.Save(ctx, slug, snap) //nolint:wrapcheck
}

// casTestServer wires the minimal Server invokeWithCAS touches: a real sandbox
// (generous rate limit so retries don't trip it) plus the given state store.
func casTestServer(st state.Store) *Server {
	return &Server{
		sandbox: sandbox.New(sandbox.Config{CPUTimeout: 2 * time.Second, RPS: 1000, RPSBurst: 1000}),
		state:   st,
	}
}

// kvWriteHandler marks the snapshot dirty on every run, forcing the Save path.
const kvWriteHandler = `module.exports = function (req) { kv.incr("hits"); return response.json({ok:true}); };`

func TestInvokeWithCAS_RetriesThroughConflicts(t *testing.T) {
	cs := &conflictingStore{Store: state.NewMemory(), conflictsLeft: 2}
	s := casTestServer(cs)

	resp, err := s.invokeWithCAS(context.Background(), "site", "fn", kvWriteHandler, sandbox.Request{Method: "POST"}, nil)
	if err != nil {
		t.Fatalf("invokeWithCAS: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}
	if cs.saves != 3 {
		t.Errorf("saves = %d, want 3 (two conflicts + one success)", cs.saves)
	}

	// The winning run's write must actually be persisted.
	snap, err := cs.Load(context.Background(), "site")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Data["hits"] == nil {
		t.Errorf("persisted state missing the handler's write: %v", snap.Data)
	}
}

func TestInvokeWithCAS_ExhaustionSurfacesConflict(t *testing.T) {
	cs := &conflictingStore{Store: state.NewMemory(), conflictsLeft: 100}
	s := casTestServer(cs)

	_, err := s.invokeWithCAS(context.Background(), "site", "fn", kvWriteHandler, sandbox.Request{Method: "POST"}, nil)
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("err = %v, want state.ErrConflict (maps to 503)", err)
	}
	if cs.saves != maxCASRetries+1 {
		t.Errorf("saves = %d, want %d (initial + retries)", cs.saves, maxCASRetries+1)
	}
}

func TestInvokeWithCAS_CleanReadSkipsSave(t *testing.T) {
	cs := &conflictingStore{Store: state.NewMemory()}
	s := casTestServer(cs)

	const readOnly = `module.exports = function (req) { return response.json({v: kv.get("missing", "fallback")}); };`
	resp, err := s.invokeWithCAS(context.Background(), "site", "fn", readOnly, sandbox.Request{Method: "GET"}, nil)
	if err != nil {
		t.Fatalf("invokeWithCAS: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}
	if cs.saves != 0 {
		t.Errorf("saves = %d, want 0 (snapshot never dirtied)", cs.saves)
	}
}

func TestInvokeWithCAS_StatelessWithoutStore(t *testing.T) {
	s := casTestServer(nil)
	s.state = nil

	const stateless = `module.exports = function (req) { return response.json({ok:true}); };`
	resp, err := s.invokeWithCAS(context.Background(), "site", "fn", stateless, sandbox.Request{Method: "GET"}, nil)
	if err != nil {
		t.Fatalf("invokeWithCAS: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}
}
