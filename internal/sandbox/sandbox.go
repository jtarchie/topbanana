// Package sandbox runs user-authored JavaScript handlers (the bodies of
// /api/* requests) inside a goja Runtime with no host I/O. The agent writes
// these handlers; this package is what makes them safe to execute.
//
// One Manager handles all sites in the process. Each invocation gets a fresh
// Runtime so per-call mutations to globals can't leak across requests or
// across sites. Hard limits are enforced by the host: CPU via Runtime.Interrupt,
// memory advisory via SetMaxCallStackSize, body size and response size as
// byte caps.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/dop251/goja"

	"github.com/jtarchie/topbanana/internal/state"
)

const (
	defaultCPUTimeout    = 250 * time.Millisecond
	defaultBodyLimit     = 256 * 1024
	defaultResponseLimit = 1 << 20 // 1 MiB
	defaultRPS           = 10
	defaultRPSBurst      = 20
	defaultMaxCallStack  = 1 << 10 // 1024 frames
)

// Config configures a Manager. The zero value is valid and uses the defaults.
type Config struct {
	CPUTimeout    time.Duration
	BodyLimit     int
	ResponseLimit int
	RPS           float64
	RPSBurst      int
}

func (c *Config) applyDefaults() {
	if c.CPUTimeout <= 0 {
		c.CPUTimeout = defaultCPUTimeout
	}
	if c.BodyLimit <= 0 {
		c.BodyLimit = defaultBodyLimit
	}
	if c.ResponseLimit <= 0 {
		c.ResponseLimit = defaultResponseLimit
	}
	if c.RPS <= 0 {
		c.RPS = defaultRPS
	}
	if c.RPSBurst <= 0 {
		c.RPSBurst = defaultRPSBurst
	}
}

// Manager is the entry point. Construct once; safe for concurrent use.
type Manager struct {
	cfg Config
	lim *limiters
}

func New(cfg Config) *Manager {
	cfg.applyDefaults()
	return &Manager{cfg: cfg, lim: newLimiters(cfg.RPS, cfg.RPSBurst)}
}

// Request is the value passed to the JS handler as its `request` argument.
// Body is the raw bytes (already size-capped). Form and JSON are pre-parsed
// when the Content-Type matches; functions can also reach for Body directly.
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Form    map[string]string `json:"form,omitempty"`
	JSON    any               `json:"json,omitempty"`
}

// Response is what a handler returns. ContentType defaults to text/plain when
// empty so the caller never has to deal with an unset Content-Type header.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
	Headers     map[string]string
}

// Errors classify sandbox-level failures so the caller can decide on the right
// HTTP status. Function-level exceptions are converted to a 500 response with
// the message in slog rather than returned as a Go error.
var (
	ErrRateLimit        = errors.New("rate limited")
	ErrTimeout          = errors.New("execution timed out")
	ErrCompile          = errors.New("compile error")
	ErrNoHandler        = errors.New("no handler exported")
	ErrResponseTooLarge = errors.New("response body exceeds limit")
)

// LogFn receives each console.log line so the caller can stream it (e.g.,
// over SSE during a test invocation) or persist it. May be nil.
type LogFn func(level, line string)

// InvokeRequest bundles the per-call inputs to Invoke. slug stays a separate
// argument because it scopes the rate limiter together with Name.
type InvokeRequest struct {
	Name     string          // handler name — rate-limit key and compile label
	Source   string          // JS module source to compile and run
	Request  Request         // the HTTP-ish request handed to the handler
	Snapshot *state.Snapshot // when non-nil, exposes a mutable kv.* global
	Log      LogFn           // console.log sink; may be nil
}

// Invoke compiles and runs in.Source against in.Request. slug and in.Name scope
// the rate limiter so a hot site can't starve others. If in.Snapshot is
// non-nil, the handler gets a `kv.*` global that mutates it; the caller is
// responsible for persisting via state.Store.Save afterwards.
func (m *Manager) Invoke(ctx context.Context, slug string, in InvokeRequest) (Response, error) {
	if !m.lim.allow(slug, in.Name) {
		return Response{}, ErrRateLimit
	}

	prog, err := goja.Compile(in.Name, in.Source, false)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %s", ErrCompile, err.Error())
	}

	vm := goja.New()
	vm.SetMaxCallStackSize(defaultMaxCallStack)

	stripUnsafeGlobals(vm)

	moduleObj, err := installGlobals(vm, in.Snapshot, in.Log)
	if err != nil {
		return Response{}, err
	}

	// Hard CPU timer. Interrupt sends a panic into the running script; goja
	// surfaces it as an error from RunProgram, which we convert to ErrTimeout.
	timer := time.AfterFunc(m.cfg.CPUTimeout, func() {
		vm.Interrupt("execution timed out")
	})
	defer timer.Stop()

	_, err = vm.RunProgram(prog)
	if err != nil {
		if isInterruptErr(err) {
			return Response{}, ErrTimeout
		}
		return Response{}, fmt.Errorf("run script: %w", err)
	}

	handler, err := resolveHandler(vm, moduleObj)
	if err != nil {
		return Response{}, err
	}

	reqVal, err := buildRequestValue(vm, in.Request)
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}

	// Re-arm the interrupt for the handler call. Reuse the same timer
	// instance — Stop+Reset isn't safe pre-Go 1.23, but we re-create.
	timer.Stop()
	timer = time.AfterFunc(m.cfg.CPUTimeout, func() {
		vm.Interrupt("execution timed out")
	})
	defer timer.Stop()

	result, err := handler(goja.Undefined(), reqVal)
	if err != nil {
		if isInterruptErr(err) {
			return Response{}, ErrTimeout
		}
		// Handler threw an exception. Surface as a 500 with the error message
		// in the body — useful for debugging via the test endpoint.
		return Response{
			Status:      500,
			ContentType: "text/plain; charset=utf-8",
			Body:        []byte("handler error: " + err.Error()),
		}, nil
	}

	return marshalResponse(vm, result, m.cfg.ResponseLimit)
}

// installGlobals wires the host-provided JS globals (module/exports, console,
// response, escape, validate, optional kv) onto vm in one place. Pulled out of
// Invoke to keep that function under the cyclomatic limit; each installer is
// independent of the others, so the order only matters for kv (which depends
// on snap being non-nil).
func installGlobals(vm *goja.Runtime, snap *state.Snapshot, log LogFn) (*goja.Object, error) {
	moduleObj, err := installModule(vm)
	if err != nil {
		return nil, err
	}
	err = installConsole(vm, log)
	if err != nil {
		return nil, fmt.Errorf("install console: %w", err)
	}
	err = installResponseBuilder(vm)
	if err != nil {
		return nil, fmt.Errorf("install response: %w", err)
	}
	err = installEscape(vm)
	if err != nil {
		return nil, fmt.Errorf("install escape: %w", err)
	}
	err = installValidate(vm)
	if err != nil {
		return nil, fmt.Errorf("install validate: %w", err)
	}
	if snap != nil {
		err = installKV(vm, snap)
		if err != nil {
			return nil, fmt.Errorf("install kv: %w", err)
		}
	}
	return moduleObj, nil
}

// installModule wires up the CommonJS `module` / `exports` globals so handlers
// can use `module.exports = fn` or `exports.handler = fn`. Returns the
// moduleObj so the caller can inspect `module.exports` after the script runs.
func installModule(vm *goja.Runtime) (*goja.Object, error) {
	exportsObj := vm.NewObject()
	moduleObj := vm.NewObject()
	err := moduleObj.Set("exports", exportsObj)
	if err != nil {
		return nil, fmt.Errorf("module init: %w", err)
	}
	err = vm.Set("module", moduleObj)
	if err != nil {
		return nil, fmt.Errorf("set module: %w", err)
	}
	err = vm.Set("exports", exportsObj)
	if err != nil {
		return nil, fmt.Errorf("set exports: %w", err)
	}
	return moduleObj, nil
}

// stripUnsafeGlobals removes JS built-ins that let a handler execute strings
// as code or evade our static lint. Other dangerous surfaces (process, require,
// fetch, setTimeout, WebAssembly) aren't exposed by goja in the first place,
// so they're absent unless we explicitly Set them.
func stripUnsafeGlobals(vm *goja.Runtime) {
	g := vm.GlobalObject()
	for _, name := range []string{"eval", "Function", "Proxy", "Reflect"} {
		// Errors here mean the global wasn't present; that's fine.
		_ = g.Delete(name)
	}
}

// installConsole exposes console.log/info/warn/error. Each routes through
// LogFn (if non-nil) and otherwise discards output.
func installConsole(vm *goja.Runtime, log LogFn) error {
	console := vm.NewObject()
	emit := func(level string) func(call goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			if log == nil {
				return goja.Undefined()
			}
			parts := make([]string, 0, len(call.Arguments))
			for _, a := range call.Arguments {
				parts = append(parts, a.String())
			}
			log(level, strings.Join(parts, " "))
			return goja.Undefined()
		}
	}
	for _, level := range []string{"log", "info", "warn", "error", "debug"} {
		err := console.Set(level, emit(level))
		if err != nil {
			return fmt.Errorf("console.%s: %w", level, err)
		}
	}
	return vm.Set("console", console) //nolint:wrapcheck
}

// installResponseBuilder provides the `response` global with helpers that
// produce a plain object the host then marshals into a Response struct. We
// intentionally don't expose a mutable response — pure builders are easier to
// reason about and to test.
func installResponseBuilder(vm *goja.Runtime) error {
	resp := vm.NewObject()

	mkResponse := func(status int, contentType string, body any, headers map[string]string) goja.Value {
		obj := vm.NewObject()
		_ = obj.Set("status", status)
		if contentType != "" {
			_ = obj.Set("contentType", contentType)
		}
		_ = obj.Set("body", body)
		if len(headers) > 0 {
			h := vm.NewObject()
			for k, v := range headers {
				_ = h.Set(k, v)
			}
			_ = obj.Set("headers", h)
		}
		return obj
	}

	_ = resp.Set("json", func(call goja.FunctionCall) goja.Value {
		v := call.Argument(0).Export()
		// Optional status second arg — response.json({errors}, 400) is the
		// shape the docs and the contact-form skeleton ship, so dropping it
		// silently turned every validation failure into a 200.
		status := 200
		if !goja.IsUndefined(call.Argument(1)) {
			status = int(call.Argument(1).ToInteger())
		}
		return mkResponse(status, "application/json", v, nil)
	})
	_ = resp.Set("html", func(call goja.FunctionCall) goja.Value {
		return mkResponse(200, "text/html; charset=utf-8", call.Argument(0).String(), nil)
	})
	_ = resp.Set("text", func(call goja.FunctionCall) goja.Value {
		return mkResponse(200, "text/plain; charset=utf-8", call.Argument(0).String(), nil)
	})
	_ = resp.Set("redirect", func(call goja.FunctionCall) goja.Value {
		loc := call.Argument(0).String()
		code := 303
		if !goja.IsUndefined(call.Argument(1)) {
			code = int(call.Argument(1).ToInteger())
		}
		return mkResponse(code, "", "", map[string]string{"Location": loc})
	})
	_ = resp.Set("status", func(call goja.FunctionCall) goja.Value {
		code := int(call.Argument(0).ToInteger())
		body := ""
		if !goja.IsUndefined(call.Argument(1)) {
			body = call.Argument(1).String()
		}
		return mkResponse(code, "text/plain; charset=utf-8", body, nil)
	})

	return vm.Set("response", resp) //nolint:wrapcheck
}

// resolveHandler picks the callable function out of the module.exports object.
// We accept three shapes the LLM commonly emits:
//
//	module.exports = function (req) { ... }
//	module.exports = { handler: function (req) { ... } }
//	exports.handler = function (req) { ... }
//
// The last two collapse to the same shape (an object with a `handler` key)
// because exports and module.exports start as the same reference.
func resolveHandler(vm *goja.Runtime, moduleObj *goja.Object) (goja.Callable, error) {
	exportsVal := moduleObj.Get("exports")
	if exportsVal == nil || goja.IsUndefined(exportsVal) || goja.IsNull(exportsVal) {
		return nil, ErrNoHandler
	}
	if fn, ok := goja.AssertFunction(exportsVal); ok {
		return fn, nil
	}
	obj := exportsVal.ToObject(vm)
	if obj == nil {
		return nil, ErrNoHandler
	}
	if fn, ok := goja.AssertFunction(obj.Get("handler")); ok {
		return fn, nil
	}
	return nil, ErrNoHandler
}

func buildRequestValue(vm *goja.Runtime, req Request) (goja.Value, error) {
	obj := vm.NewObject()
	_ = obj.Set("method", req.Method)
	_ = obj.Set("path", req.Path)
	_ = obj.Set("body", req.Body)
	q := vm.NewObject()
	for k, v := range req.Query {
		_ = q.Set(k, v)
	}
	_ = obj.Set("query", q)
	h := vm.NewObject()
	for k, v := range req.Headers {
		_ = h.Set(k, v)
	}
	_ = obj.Set("headers", h)
	if req.Form != nil {
		f := vm.NewObject()
		for k, v := range req.Form {
			_ = f.Set(k, v)
		}
		_ = obj.Set("form", f)
	}
	if req.JSON != nil {
		_ = obj.Set("json", req.JSON)
	}
	return obj, nil
}

// marshalResponse converts the handler's return value into a Response. The
// handler may return:
//   - nothing → 204 No Content
//   - a string → 200 with text/html
//   - an object with {status, contentType, body, headers}
//
//nolint:cyclop // dispatch over response shape; each branch is trivial.
func marshalResponse(vm *goja.Runtime, v goja.Value, limit int) (Response, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return Response{Status: 204}, nil
	}

	if s, ok := v.Export().(string); ok {
		body := []byte(s)
		if len(body) > limit {
			return Response{}, ErrResponseTooLarge
		}
		return Response{Status: 200, ContentType: "text/html; charset=utf-8", Body: body}, nil
	}

	obj := v.ToObject(vm)
	if obj == nil {
		return Response{Status: 204}, nil
	}

	resp := Response{Status: 200, ContentType: "text/plain; charset=utf-8"}
	if statusVal := obj.Get("status"); statusVal != nil && !goja.IsUndefined(statusVal) {
		resp.Status = int(statusVal.ToInteger())
	}
	if ctVal := obj.Get("contentType"); ctVal != nil && !goja.IsUndefined(ctVal) && !goja.IsNull(ctVal) {
		if ct := ctVal.String(); ct != "" {
			resp.ContentType = ct
		}
	}
	bodyVal := obj.Get("body")
	switch {
	case bodyVal == nil, goja.IsUndefined(bodyVal), goja.IsNull(bodyVal):
		resp.Body = nil
	default:
		switch resp.ContentType {
		case "application/json":
			b, err := json.Marshal(bodyVal.Export())
			if err != nil {
				return Response{}, fmt.Errorf("encode json response: %w", err)
			}
			resp.Body = b
		default:
			resp.Body = []byte(bodyVal.String())
		}
	}
	if len(resp.Body) > limit {
		return Response{}, ErrResponseTooLarge
	}
	if hVal := obj.Get("headers"); hVal != nil && !goja.IsUndefined(hVal) && !goja.IsNull(hVal) {
		resp.Headers = map[string]string{}
		hObj := hVal.ToObject(vm)
		if hObj != nil {
			for _, k := range hObj.Keys() {
				resp.Headers[k] = hObj.Get(k).String()
			}
		}
	}
	return resp, nil
}

func isInterruptErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTimeout) {
		return true
	}
	var ie *goja.InterruptedError
	return errors.As(err, &ie)
}

// installEscape exposes a global `escape(s)` that HTML-escapes a string so
// handlers can safely concatenate user-supplied values into response.html(...)
// or into JSON the client will assign to .innerHTML. Matches Go's
// html/template auto-escaping byte-for-byte.
func installEscape(vm *goja.Runtime) error {
	fn := func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return vm.ToValue("")
		}
		in := call.Argument(0)
		if goja.IsUndefined(in) || goja.IsNull(in) {
			return vm.ToValue("")
		}
		return vm.ToValue(template.HTMLEscapeString(in.String()))
	}
	return vm.Set("escape", fn) //nolint:wrapcheck
}

// installValidate exposes a global `validate(input, schema)` that runs
// schema-driven validation against an input object (typically request.form or
// request.json) and returns either { ok: true, data } or { ok: false, errors }.
// Unknown fields in input are dropped (strong-parameters posture) so handlers
// can pass request.form directly without worrying about extras.
func installValidate(vm *goja.Runtime) error {
	fn := func(call goja.FunctionCall) goja.Value {
		inputMap := exportObject(call.Argument(0))
		schemaMap := exportObject(call.Argument(1))
		if schemaMap == nil {
			return mkValidateResult(vm, nil, []validationError{{Field: "_schema", Message: "schema must be an object"}})
		}
		data, errs := validateInput(inputMap, schemaMap)
		return mkValidateResult(vm, data, errs)
	}
	return vm.Set("validate", fn) //nolint:wrapcheck
}

// exportObject converts a goja value to a plain map[string]any. Returns nil
// when the value is undefined/null or not an object — callers decide whether
// that's a schema error or "no input".
func exportObject(v goja.Value) map[string]any {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	exported := v.Export()
	if m, ok := exported.(map[string]any); ok {
		return m
	}
	return nil
}

func mkValidateResult(vm *goja.Runtime, data map[string]any, errs []validationError) goja.Value {
	obj := vm.NewObject()
	if len(errs) > 0 {
		_ = obj.Set("ok", false)
		arr := make([]any, 0, len(errs))
		for _, e := range errs {
			arr = append(arr, map[string]any{"field": e.Field, "message": e.Message})
		}
		_ = obj.Set("errors", arr)
		return obj
	}
	_ = obj.Set("ok", true)
	if data == nil {
		data = map[string]any{}
	}
	_ = obj.Set("data", data)
	return obj
}
