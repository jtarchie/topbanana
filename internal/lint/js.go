package lint

import (
	"fmt"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/unistring"
)

const (
	// jsMaxBytes caps the size of a function source file. The agent only ever
	// emits short handlers; this is a sanity bound, not a real performance
	// constraint.
	jsMaxBytes = 64 * 1024

	// functionsDir is the only place functions may live. Files outside this
	// prefix are rejected so the agent can't smuggle JS into HTML paths.
	functionsDir = "functions/"
)

// forbiddenIdentifiers are JS identifiers that, if referenced in handler code,
// indicate an attempt to escape the sandbox. The list is conservative: goja
// doesn't expose most of these to the runtime in the first place (no `process`,
// no `require`, no `fetch`), but a static check turns "the agent tried" into
// a clear error before we ever execute the script.
//
// We do not enforce a full allowlist here because the goja host bindings are
// already curated — there's no `process` or `fetch` to call even if the lint
// missed them. This is denylist as defense in depth, not as the primary gate.
var forbiddenIdentifiers = map[string]string{
	"eval":              "eval is forbidden",
	"Function":          "Function constructor is forbidden",
	"require":           "require is forbidden (only module.exports = ... is allowed)",
	"process":           "process is not available in the sandbox",
	"globalThis":        "globalThis is forbidden",
	"WebAssembly":       "WebAssembly is forbidden",
	"setTimeout":        "setTimeout is not available",
	"setInterval":       "setInterval is not available",
	"setImmediate":      "setImmediate is not available",
	"fetch":             "network access is not available",
	"XMLHttpRequest":    "network access is not available",
	"WebSocket":         "network access is not available",
	"importScripts":     "imports are not available",
	"Worker":            "workers are not available",
	"SharedArrayBuffer": "SharedArrayBuffer is not available",
	"Atomics":           "Atomics are not available",
	"Proxy":             "Proxy is forbidden",
	"Reflect":           "Reflect is forbidden",
}

// JSFile validates a single function source. Returns an empty slice on success.
// Caller decides whether to surface the errors as lint failures or hard
// rejections; the build retry loop uses them as fix-prompts.
func JSFile(filename, source string) []Error {
	if !strings.HasPrefix(filename, functionsDir) || !strings.HasSuffix(filename, ".js") {
		return []Error{{File: filename, Message: "JS files must live under functions/ and end in .js"}}
	}
	if len(source) > jsMaxBytes {
		return []Error{{File: filename, Message: fmt.Sprintf("function source %d bytes exceeds %d byte cap", len(source), jsMaxBytes)}}
	}

	prog, err := parser.ParseFile(nil, filename, source, 0)
	if err != nil {
		return []Error{{File: filename, Message: fmt.Sprintf("parse error: %s", err)}}
	}

	var errs []Error
	w := &jsWalker{filename: filename, errs: &errs}
	w.walkProgram(prog)

	if !w.exportsHandler {
		errs = append(errs, Error{
			File:    filename,
			Message: "no handler exported (use module.exports = function(request) {...})",
		})
	}

	return errs
}

// jsWalker traverses the AST recording two things: any forbidden identifier
// reference (regardless of scope — these names are dangerous wherever they
// appear), and whether the source ever assigns to module.exports or
// exports.handler. The latter answers "is there a handler to call?" before we
// load the script into a VM.
type jsWalker struct {
	filename       string
	errs           *[]Error
	exportsHandler bool

	// Optional collector hooks — nil by default so JSFile's behavior is
	// untouched. collectJSFacts (inline scripts) and jsFileLiterals
	// (functions/*.js) set them to harvest facts from the same traversal the
	// forbidden-identifier check uses. collectOnly suppresses that check,
	// because browser-side JS legitimately calls fetch/setTimeout/etc.
	collectOnly bool
	onString    func(string)              // string literals + expressionless templates
	onCall      func(*ast.CallExpression) // every call expression, before its children
	onTopName   func(string)              // top-level declared / window.X-assigned names
}

func (w *jsWalker) addErr(msg string) {
	*w.errs = append(*w.errs, Error{File: w.filename, Message: msg})
}

func (w *jsWalker) checkIdentifier(name unistring.String) {
	if w.collectOnly {
		return
	}
	if msg, bad := forbiddenIdentifiers[string(name)]; bad {
		w.addErr(msg)
	}
}

func (w *jsWalker) walkProgram(p *ast.Program) {
	for _, s := range p.Body {
		w.noteTopLevelNames(s)
		w.walkStmt(s)
	}
}

// noteTopLevelNames reports script-level declared names to the onTopName
// hook. Top-level function/var/let/const names in a classic inline script
// live on the page's global lexical environment, so inline event handlers
// (onclick="name()") can call them. (Module-script top-level names are not
// true globals; counting them anyway only suppresses errors — the right bias
// for an "undefined handler" check.)
func (w *jsWalker) noteTopLevelNames(s ast.Statement) {
	if w.onTopName == nil {
		return
	}
	switch n := s.(type) {
	case *ast.FunctionDeclaration:
		if n.Function != nil && n.Function.Name != nil {
			w.onTopName(string(n.Function.Name.Name))
		}
	case *ast.VariableStatement:
		for _, b := range n.List {
			w.noteBindingName(b)
		}
	case *ast.LexicalDeclaration:
		for _, b := range n.List {
			w.noteBindingName(b)
		}
	}
}

// noteBindingName reports a binding's name when it is a plain identifier;
// destructuring targets are skipped.
func (w *jsWalker) noteBindingName(b *ast.Binding) {
	if b == nil {
		return
	}
	if id, ok := b.Target.(*ast.Identifier); ok {
		w.onTopName(string(id.Name))
	}
}

//nolint:gocyclo,funlen,cyclop // straightforward switch over AST node types; splitting hurts readability.
func (w *jsWalker) walkStmt(s ast.Statement) {
	if s == nil {
		return
	}
	switch n := s.(type) {
	case *ast.BlockStatement:
		for _, x := range n.List {
			w.walkStmt(x)
		}
	case *ast.ExpressionStatement:
		w.walkExpr(n.Expression)
	case *ast.IfStatement:
		w.walkExpr(n.Test)
		w.walkStmt(n.Consequent)
		w.walkStmt(n.Alternate)
	case *ast.ReturnStatement:
		w.walkExpr(n.Argument)
	case *ast.ForStatement:
		w.walkForInitializer(n.Initializer)
		w.walkExpr(n.Test)
		w.walkExpr(n.Update)
		w.walkStmt(n.Body)
	case *ast.ForInStatement:
		w.walkForInto(n.Into)
		w.walkExpr(n.Source)
		w.walkStmt(n.Body)
	case *ast.ForOfStatement:
		w.walkForInto(n.Into)
		w.walkExpr(n.Source)
		w.walkStmt(n.Body)
	case *ast.WhileStatement:
		w.walkExpr(n.Test)
		w.walkStmt(n.Body)
	case *ast.DoWhileStatement:
		w.walkExpr(n.Test)
		w.walkStmt(n.Body)
	case *ast.SwitchStatement:
		w.walkExpr(n.Discriminant)
		for _, c := range n.Body {
			w.walkExpr(c.Test)
			for _, x := range c.Consequent {
				w.walkStmt(x)
			}
		}
	case *ast.TryStatement:
		w.walkStmt(n.Body)
		if n.Catch != nil {
			w.walkStmt(n.Catch.Body)
		}
		if n.Finally != nil {
			w.walkStmt(n.Finally)
		}
	case *ast.ThrowStatement:
		w.walkExpr(n.Argument)
	case *ast.LabelledStatement:
		w.walkStmt(n.Statement)
	case *ast.VariableStatement:
		for _, b := range n.List {
			w.walkBinding(b)
		}
	case *ast.LexicalDeclaration:
		for _, b := range n.List {
			w.walkBinding(b)
		}
	case *ast.FunctionDeclaration:
		w.walkFunction(n.Function)
	case *ast.ClassDeclaration:
		// Classes are allowed; their internals are walked as expressions.
		w.walkExpr(n.Class)
	}
}

func (w *jsWalker) walkForInitializer(i ast.ForLoopInitializer) {
	switch v := i.(type) {
	case *ast.ForLoopInitializerExpression:
		w.walkExpr(v.Expression)
	case *ast.ForLoopInitializerVarDeclList:
		for _, b := range v.List {
			w.walkBinding(b)
		}
	case *ast.ForLoopInitializerLexicalDecl:
		for _, b := range v.LexicalDeclaration.List {
			w.walkBinding(b)
		}
	}
}

func (w *jsWalker) walkBinding(b *ast.Binding) {
	if b == nil {
		return
	}
	w.walkExpr(b.Initializer)
}

func (w *jsWalker) walkForInto(i ast.ForInto) {
	switch v := i.(type) {
	case *ast.ForIntoExpression:
		w.walkExpr(v.Expression)
	case *ast.ForIntoVar:
		w.walkExpr(v.Binding)
	}
}

func (w *jsWalker) walkFunction(fn *ast.FunctionLiteral) {
	if fn == nil {
		return
	}
	if fn.Body != nil {
		w.walkStmt(fn.Body)
	}
}

//nolint:gocyclo,funlen,cyclop // dispatch over expression node types; cyclomatic complexity is in the data.
func (w *jsWalker) walkExpr(e ast.Expression) {
	if e == nil {
		return
	}
	switch n := e.(type) {
	case *ast.Identifier:
		w.checkIdentifier(n.Name)
	case *ast.StringLiteral:
		if w.onString != nil {
			w.onString(string(n.Value))
		}
	case *ast.AssignExpression:
		w.checkAssignsHandler(n)
		w.noteWindowAssign(n)
		w.walkExpr(n.Left)
		w.walkExpr(n.Right)
	case *ast.BinaryExpression:
		w.walkExpr(n.Left)
		w.walkExpr(n.Right)
	case *ast.UnaryExpression:
		w.walkExpr(n.Operand)
	case *ast.ConditionalExpression:
		w.walkExpr(n.Test)
		w.walkExpr(n.Consequent)
		w.walkExpr(n.Alternate)
	case *ast.CallExpression:
		if w.onCall != nil {
			w.onCall(n)
		}
		w.walkExpr(n.Callee)
		for _, a := range n.ArgumentList {
			w.walkExpr(a)
		}
	case *ast.NewExpression:
		w.walkExpr(n.Callee)
		for _, a := range n.ArgumentList {
			w.walkExpr(a)
		}
	case *ast.DotExpression:
		// Member access on a host-supplied object is fine (e.g. console.log,
		// response.json). Just walk the receiver — Identifier check on the
		// root catches forbidden names.
		w.walkExpr(n.Left)
	case *ast.BracketExpression:
		w.walkExpr(n.Left)
		w.walkExpr(n.Member)
	case *ast.PrivateDotExpression:
		w.walkExpr(n.Left)
	case *ast.SequenceExpression:
		for _, x := range n.Sequence {
			w.walkExpr(x)
		}
	case *ast.ObjectLiteral:
		for _, p := range n.Value {
			if pk, ok := p.(*ast.PropertyKeyed); ok {
				w.walkExpr(pk.Value)
			}
		}
	case *ast.ArrayLiteral:
		for _, v := range n.Value {
			w.walkExpr(v)
		}
	case *ast.FunctionLiteral:
		w.walkFunction(n)
	case *ast.ArrowFunctionLiteral:
		switch body := n.Body.(type) {
		case *ast.BlockStatement:
			w.walkStmt(body)
		case *ast.ExpressionBody:
			w.walkExpr(body.Expression)
		}
	case *ast.TemplateLiteral:
		w.noteTemplateString(n)
		for _, x := range n.Expressions {
			w.walkExpr(x)
		}
	case *ast.ClassLiteral:
		// Walk method bodies; class names themselves don't get checked.
		for _, el := range n.Body {
			if md, ok := el.(*ast.MethodDefinition); ok {
				w.walkFunction(md.Body)
			}
		}
	case *ast.AwaitExpression:
		w.walkExpr(n.Argument)
	}
}

// noteTemplateString reports an expressionless template literal to the
// onString hook — `/api/orders` and "..." are the same constant to a reader,
// so the collectors treat them the same.
func (w *jsWalker) noteTemplateString(n *ast.TemplateLiteral) {
	if w.onString == nil || len(n.Expressions) > 0 || len(n.Elements) == 0 {
		return
	}
	var b strings.Builder
	for _, el := range n.Elements {
		b.WriteString(string(el.Parsed))
	}
	w.onString(b.String())
}

// noteWindowAssign reports `window.name = ...` assignments to the onTopName
// hook — inline event handlers can call names attached to window from any
// scope, not just top-level declarations.
func (w *jsWalker) noteWindowAssign(a *ast.AssignExpression) {
	if w.onTopName == nil || a == nil || a.Operator.String() != "=" {
		return
	}
	left, ok := a.Left.(*ast.DotExpression)
	if !ok {
		return
	}
	if root, isIdent := left.Left.(*ast.Identifier); isIdent && string(root.Name) == "window" {
		w.onTopName(string(left.Identifier.Name))
	}
}

// jsFileLiterals extracts every string literal from a function source for the
// unreferenced-page scan — functions reach pages via literals like
// response.redirect("/thanks.html"). Parse failures return nil: JSFile has
// already reported the error, and a missing reference here only risks an
// extra lint message, never a broken build.
func jsFileLiterals(filename, source string) []string {
	prog, err := parser.ParseFile(nil, filename, source, 0)
	if err != nil {
		return nil
	}
	var lits []string
	var discard []Error
	w := &jsWalker{
		filename:    filename,
		errs:        &discard,
		collectOnly: true,
		onString:    func(s string) { lits = append(lits, s) },
	}
	w.walkProgram(prog)
	return lits
}

// checkAssignsHandler flips exportsHandler to true if this assignment matches
// one of the well-known module.exports / exports.handler shapes. We don't try
// to be exhaustive — the goal is to fail fast when the agent forgot to export
// anything at all, not to forbid creative export shapes.
func (w *jsWalker) checkAssignsHandler(a *ast.AssignExpression) {
	if a == nil || a.Operator.String() != "=" {
		return
	}
	left, ok := a.Left.(*ast.DotExpression)
	if !ok {
		return
	}
	root, prop := unwrapDot(left)
	switch {
	case root == "module" && prop == "exports":
		w.exportsHandler = true
	case root == "exports":
		w.exportsHandler = true
	}
}

// unwrapDot walks the root and last-property name out of a chained
// MemberExpression. Returns empty strings when the chain doesn't terminate in
// an Identifier root (e.g. `foo().bar`).
func unwrapDot(d *ast.DotExpression) (root, lastProp string) {
	lastProp = string(d.Identifier.Name)
	switch x := d.Left.(type) {
	case *ast.Identifier:
		return string(x.Name), lastProp
	case *ast.DotExpression:
		r, _ := unwrapDot(x)
		return r, lastProp
	}
	return "", lastProp
}
