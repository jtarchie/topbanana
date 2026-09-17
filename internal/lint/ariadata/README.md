# ariadata — the WAI-ARIA roles model, as data

`roles.json` is vendored **verbatim** from [aria-query][] v5.3.2
(`scripts/roles.json`, Apache-2.0 — see `LICENSE` in this directory). It is the
generator input aria-query itself uses, which makes it the one machine-readable
form of the ARIA 1.2 Roles Model: 139 roles, each with its allowed, required,
and prohibited states and properties, its required parent and child roles, and
whether it takes an accessible name.

That table is the whole reason ARIA linting looks like a big project. With it,
`aria.go` is map lookups over the parse tree `internal/lint` already walks — no
browser, no DOM, no Node, no new runtime dependency.

## Why vendor the raw file instead of a trimmed Go table

Upgrading is `curl` + `go test ./internal/lint/...`, and a diff of the vendored
file is a diff of the spec. A hand-trimmed Go literal would drift, and the
provenance of each field would stop being checkable.

```sh
V=v5.3.2
curl -sfL "https://raw.githubusercontent.com/A11yance/aria-query/$V/scripts/roles.json" -o roles.json
curl -sfL "https://raw.githubusercontent.com/A11yance/aria-query/$V/LICENSE"            -o LICENSE
```

Bump the version in this README when you do, and expect `data_test.go` to fail
loudly if a field this package depends on changes shape.

## Two quirks of the upstream data this package normalizes

- **`none` is empty upstream.** It is a synonym for `presentation` but ships
  with no props, no superclass, and no prohibited list. Taken literally, every
  `role="none"` element would look like it permits nothing. `Get` aliases it to
  `presentation`.
- **`requiredProps` mixes two meanings.** A bare string (`"aria-checked"` on
  `checkbox`) is a genuine requirement. A pair (`["aria-level", "2"]` on
  `heading`) declares an *implicit default*, so omitting the attribute is
  legal. Only bare strings land in `Role.Required` — the same line axe-core's
  `aria-required-attr` rule draws.

## What is deliberately not taken from this file

`relatedConcepts` nominally maps HTML elements to roles, but it encodes the
conditional cases (six different `input` entries for `combobox`) in a shape
that varies per role. `implicit_roles.go` carries a small hand-written HTML-AAM
subset instead — narrower, and honest about what it covers.

[aria-query]: https://github.com/A11yance/aria-query
