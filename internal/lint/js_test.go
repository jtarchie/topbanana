package lint

import (
	"strings"
	"testing"
)

func TestJSFile_HappyPath(t *testing.T) {
	src := `
		module.exports = function (request) {
			console.log("got", request.body);
			return response.json({ ok: true });
		};
	`
	errs := JSFile("functions/submit.js", src)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

func TestJSFile_ExportsHandlerObject(t *testing.T) {
	src := `
		exports.handler = function (request) {
			return response.text("ok");
		};
	`
	errs := JSFile("functions/handler.js", src)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

func TestJSFile_PathOutsideFunctions(t *testing.T) {
	errs := JSFile("foo.js", "module.exports = function() {};")
	if len(errs) == 0 {
		t.Fatal("expected error for path outside functions/")
	}
}

func TestJSFile_OversizeRejected(t *testing.T) {
	big := strings.Repeat("x", jsMaxBytes+1)
	errs := JSFile("functions/big.js", big)
	if len(errs) == 0 {
		t.Fatal("expected size cap error")
	}
}

func TestJSFile_MissingHandlerRejected(t *testing.T) {
	src := `var x = 1;`
	errs := JSFile("functions/noop.js", src)
	if len(errs) == 0 {
		t.Fatal("expected no-handler error")
	}
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "no handler") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'no handler' message, got: %v", errs)
	}
}

func TestJSFile_ForbiddenIdentifiers(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"eval", `module.exports = function(){ return eval("1+1"); };`},
		{"Function", `module.exports = function(){ return new Function("return 1")(); };`},
		{"require", `var fs = require("fs"); module.exports = function(){};`},
		{"process", `module.exports = function(){ return process.env.PATH; };`},
		{"fetch", `module.exports = async function(){ return await fetch("http://x"); };`},
		{"setTimeout", `module.exports = function(){ setTimeout(function(){}, 1); };`},
		{"WebAssembly", `module.exports = function(){ return WebAssembly; };`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := JSFile("functions/x.js", tc.src)
			if len(errs) == 0 {
				t.Fatalf("expected forbidden identifier %q to be flagged", tc.name)
			}
		})
	}
}

func TestJSFile_ParseError(t *testing.T) {
	errs := JSFile("functions/x.js", "module.exports = function() { return")
	if len(errs) == 0 {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(errs[0].Message, "parse error") {
		t.Fatalf("expected 'parse error' message, got: %v", errs[0])
	}
}

func TestJSFile_ConsoleAndJSONFine(t *testing.T) {
	// Make sure common safe globals don't trip the denylist.
	src := `
		module.exports = function (request) {
			var data = JSON.parse(request.body || "{}");
			console.log("data", data);
			var s = encodeURIComponent(data.name || "");
			return response.json({ encoded: s, ts: Date.now() });
		};
	`
	errs := JSFile("functions/safe.js", src)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}
