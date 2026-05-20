package sandbox

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dop251/goja"

	"github.com/jtarchie/bloomhollow/internal/state"
)

// installKV exposes a request-scoped `kv` global to the handler. All mutations
// land on the supplied Snapshot and set Dirty=true so the caller knows to
// persist on the way out. The slug is captured here at install time — the JS
// has no surface to reach across sites.
//
// The bindings deliberately reject non-JSON values so the Save side can stay
// backend-agnostic. The agent will write nothing weirder than strings,
// numbers, booleans, arrays, and plain objects; if it tries to store a Date
// or a function, the put fails immediately with a clear error rather than
// silently breaking the flush.
//
//nolint:cyclop // five small kv.* binding closures — splitting hurts readability.
func installKV(vm *goja.Runtime, snap *state.Snapshot) error {
	kv := vm.NewObject()

	err := kv.Set("get", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		if v, ok := snap.Data[key]; ok {
			return vm.ToValue(v)
		}
		// Second arg is an optional default value; mirrors Python's dict.get.
		if !goja.IsUndefined(call.Argument(1)) {
			return call.Argument(1)
		}
		return goja.Null()
	})
	if err != nil {
		return fmt.Errorf("kv.get: %w", err)
	}

	err = kv.Set("put", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		val := normalizeForJSON(call.Argument(1).Export())
		err := validateJSONValue(val)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		snap.Data[key] = val
		snap.Dirty = true
		return goja.Undefined()
	})
	if err != nil {
		return fmt.Errorf("kv.put: %w", err)
	}

	err = kv.Set("delete", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		if _, ok := snap.Data[key]; ok {
			delete(snap.Data, key)
			snap.Dirty = true
		}
		return goja.Undefined()
	})
	if err != nil {
		return fmt.Errorf("kv.delete: %w", err)
	}

	err = kv.Set("incr", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		delta := int64(1)
		if !goja.IsUndefined(call.Argument(1)) {
			delta = call.Argument(1).ToInteger()
		}
		cur := int64(0)
		if v, ok := snap.Data[key]; ok {
			switch n := v.(type) {
			case int64:
				cur = n
			case int:
				cur = int64(n)
			case float64:
				cur = int64(n)
			default:
				panic(vm.NewGoError(fmt.Errorf("kv.incr: existing value at %q is not numeric", key)))
			}
		}
		cur += delta
		// Store as int64 so it survives a JSON round-trip without losing
		// precision on large counters.
		snap.Data[key] = cur
		snap.Dirty = true
		return vm.ToValue(cur)
	})
	if err != nil {
		return fmt.Errorf("kv.incr: %w", err)
	}

	err = kv.Set("list", func(call goja.FunctionCall) goja.Value {
		prefix := ""
		if !goja.IsUndefined(call.Argument(0)) {
			prefix = call.Argument(0).String()
		}
		keys := make([]string, 0, len(snap.Data))
		for k := range snap.Data {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		// Deterministic order so handlers can paginate or render lists
		// consistently across requests.
		sort.Strings(keys)
		entries := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			entries = append(entries, map[string]any{"key": k, "value": snap.Data[k]})
		}
		return vm.ToValue(entries)
	})
	if err != nil {
		return fmt.Errorf("kv.list: %w", err)
	}

	return vm.Set("kv", kv) //nolint:wrapcheck
}

// normalizeForJSON walks the value goja handed us and rewrites Go types that
// don't survive JSON marshal/unmarshal symmetry into ones that do. JS numbers
// always come out as float64; JS objects come out as map[string]any with
// any-typed values; that's already fine. The main thing to fix is that goja
// sometimes hands us `[]interface{}` for arrays which is fine, but
// nested types may need recursion later.
func normalizeForJSON(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, vv := range n {
			out[k] = normalizeForJSON(vv)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, vv := range n {
			out[i] = normalizeForJSON(vv)
		}
		return out
	default:
		return v
	}
}

// validateJSONValue rejects values that won't survive json.Marshal. This is
// the binding-layer typecheck the plan calls out: surface bad inputs at
// kv.put time, not later from a goroutine.
func validateJSONValue(v any) error {
	switch n := v.(type) {
	case nil, bool, string, int, int64, float64:
		return nil
	case map[string]any:
		for k, vv := range n {
			err := validateJSONValue(vv)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
		return nil
	case []any:
		for i, vv := range n {
			err := validateJSONValue(vv)
			if err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported value type %T (use string/number/bool/array/object/null)", v)
	}
}
