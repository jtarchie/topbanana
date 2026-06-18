// Package docs embeds a keyword-searchable copy of the daisyUI component
// reference (daisyUI's official, self-contained llms.txt) so the build agent
// can look up class names and modifiers on demand instead of guessing or
// reaching for a web search. The corpus is vendored under sources/ by
// `task vendor:docs` and pinned to the daisyUI release whose CSS the platform
// compiles — see DaisyUIDocsVersion.
//
// The package is a pure, dependency-free domain library: a heading-aware
// markdown chunker (chunk.go) plus a BM25-lite ranker (rank.go, index.go),
// importable by internal/agent (and, later, internal/server for an MCP tool)
// without violating the depguard rule that shared logic lives below the
// composition root. It mirrors internal/textedit, which likewise backs both
// edit surfaces with one set of pure transforms.
package docs

import _ "embed"

// DaisyUIDocsVersion is the daisyUI release whose llms.txt is vendored under
// sources/daisyui.md. Bump it with `task vendor:docs`; docs_test.go asserts it
// matches sources/VERSIONS.json AND assets.DaisyUIVersion, so the searchable
// docs can never describe a different release than the compiled component CSS.
const DaisyUIDocsVersion = "5.5.20"

//go:embed sources/daisyui.md
var daisyUIDocs string

// sourceDef is one corpus entry. This slice is the single registry of sources:
// adding Tailwind later is a new sources/<name>.md, one //go:embed var, and one
// entry here — the chunker and ranker are source-agnostic and need no change.
type sourceDef struct {
	Name    string // breadcrumb root + display label, e.g. "daisyUI"
	ID      string // stable id for Options.Source filtering, e.g. "daisyui"
	Version string // vendored release, surfaced by Sources()
	Body    string // raw markdown
}

var sources = []sourceDef{
	{Name: "daisyUI", ID: "daisyui", Version: DaisyUIDocsVersion, Body: daisyUIDocs},
}
