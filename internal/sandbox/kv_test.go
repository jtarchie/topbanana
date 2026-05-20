package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/bloomhollow/internal/state"
)

func invokeWithSnap(t *testing.T, src string, snap *state.Snapshot, req Request) (Response, []string) {
	t.Helper()
	m := New(Config{CPUTimeout: 500 * time.Millisecond})
	var logs []string
	resp, err := m.Invoke(context.Background(), "slug", "fn", src, req, snap, func(level, line string) {
		logs = append(logs, level+": "+line)
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return resp, logs
}

func TestKV_GetReturnsNullForMissing(t *testing.T) {
	snap := state.NewSnapshot()
	src := `module.exports = function() {
		var v = kv.get("missing");
		return response.json({ got: v });
	};`
	resp, _ := invokeWithSnap(t, src, snap, Request{})
	if !strings.Contains(string(resp.Body), `"got":null`) {
		t.Fatalf("body: %s", resp.Body)
	}
}

func TestKV_GetReturnsDefault(t *testing.T) {
	snap := state.NewSnapshot()
	src := `module.exports = function() {
		var v = kv.get("missing", 42);
		return response.json({ got: v });
	};`
	resp, _ := invokeWithSnap(t, src, snap, Request{})
	if !strings.Contains(string(resp.Body), `"got":42`) {
		t.Fatalf("body: %s", resp.Body)
	}
}

func TestKV_PutMarksDirtyAndPersistsInSnap(t *testing.T) {
	snap := state.NewSnapshot()
	src := `module.exports = function() {
		kv.put("name", "anna");
		kv.put("count", 3);
		return response.text("ok");
	};`
	invokeWithSnap(t, src, snap, Request{})
	if !snap.Dirty {
		t.Fatal("expected Dirty=true")
	}
	if snap.Data["name"] != "anna" {
		t.Fatalf("name: %v", snap.Data["name"])
	}
	// goja Export gives int64 for integer literals, float64 for fractionals.
	switch v := snap.Data["count"].(type) {
	case int64:
		if v != 3 {
			t.Fatalf("count: %v", v)
		}
	case float64:
		if v != 3 {
			t.Fatalf("count: %v", v)
		}
	default:
		t.Fatalf("count type: %T = %v", v, v)
	}
}

func TestKV_IncrCreatesAndIncrements(t *testing.T) {
	snap := state.NewSnapshot()
	src := `module.exports = function() {
		var a = kv.incr("hits");        // creates as 1
		var b = kv.incr("hits", 5);     // 1 + 5 = 6
		return response.json({ a: a, b: b });
	};`
	resp, _ := invokeWithSnap(t, src, snap, Request{})
	body := string(resp.Body)
	if !strings.Contains(body, `"a":1`) || !strings.Contains(body, `"b":6`) {
		t.Fatalf("body: %s", body)
	}
}

func TestKV_DeleteRemovesKey(t *testing.T) {
	snap := state.NewSnapshot()
	snap.Data["x"] = "old"
	src := `module.exports = function() {
		kv.delete("x");
		return response.json({ has: kv.get("x") });
	};`
	resp, _ := invokeWithSnap(t, src, snap, Request{})
	if !strings.Contains(string(resp.Body), `"has":null`) {
		t.Fatalf("body: %s", resp.Body)
	}
	if _, ok := snap.Data["x"]; ok {
		t.Fatal("key still present")
	}
}

func TestKV_ListReturnsPrefixSorted(t *testing.T) {
	snap := state.NewSnapshot()
	snap.Data["submission:b"] = "B"
	snap.Data["submission:a"] = "A"
	snap.Data["other"] = "X"
	src := `module.exports = function() {
		var rows = kv.list("submission:");
		return response.json(rows.map(function(r){ return r.key + "=" + r.value; }));
	};`
	resp, _ := invokeWithSnap(t, src, snap, Request{})
	body := string(resp.Body)
	if !strings.Contains(body, `"submission:a=A"`) || !strings.Contains(body, `"submission:b=B"`) {
		t.Fatalf("body: %s", body)
	}
	// "other" must not appear
	if strings.Contains(body, `"other=X"`) {
		t.Fatalf("prefix filter failed: %s", body)
	}
}

func TestKV_NoSnapshotMeansNoKVGlobal(t *testing.T) {
	// Brochure templates (no kv) should see kv as undefined.
	src := `module.exports = function() {
		return response.json({ kvType: typeof kv });
	};`
	resp, _ := invokeWithSnap(t, src, nil, Request{})
	// Should NOT contain kvType:"object" — kv shouldn't be installed.
	if strings.Contains(string(resp.Body), `"kvType":"object"`) {
		t.Fatalf("kv was installed despite nil Snapshot: %s", resp.Body)
	}
}

func TestKV_PutObjectAndList(t *testing.T) {
	snap := state.NewSnapshot()
	src := `module.exports = function() {
		kv.put("a", { name: "anna", count: 3, tags: ["x", "y"] });
		var rows = kv.list("");
		return response.json(rows);
	};`
	resp, _ := invokeWithSnap(t, src, snap, Request{})
	body := string(resp.Body)
	if !strings.Contains(body, `"name":"anna"`) {
		t.Fatalf("expected name in body: %s", body)
	}
	if !strings.Contains(body, `"tags":["x","y"]`) {
		t.Fatalf("expected tags in body: %s", body)
	}
}
