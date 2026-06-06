---
target: internal/server/templates/files.html
total_score: 28
p0_count: 0
p1_count: 1
timestamp: 2026-06-06T19-54-38Z
slug: internal-server-templates-files-html
---
#### Design Health Score — files.html (object inventory)

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visual hierarchy | 3 | A table is the right primitive; H1 + intro + table reads in one beat |
| 2 | Information rhythm | 3 | `table-zebra` gives rhythm; column widths fixed and sensible |
| 3 | Density | 4 | Highest density on the family — one row per file, four columns |
| 4 | Typography | 3 | Mono path, /70 size + date. Lowercase column heads a touch casual for forensics |
| 5 | Color discipline | 3 | `link-error` for delete is right primary-tone-of-danger; otherwise quiet |
| 6 | Mono usage | 4 | Path mono, sizes/dates plain — textbook |
| 7 | Empty state | 1 | "No files yet." — period. Doesn't teach the surface |
| 8 | Accessibility | 2 | No `<caption>`, no `scope="col"`; delete is a `<button class="link link-error">` (looks like a link) |
| 9 | Brand voice | 3 | Quiet, factual, lowercase column heads feel intentional |
| 10 | Diagnostic fitness | 2 | No filter, no sort, no size totals, no content-type column |
| **Total** | | **28/40** | **Good — needs filter + better destructive ceremony** |

#### Anti-Patterns Verdict

**LLM assessment.** Honest table layout, on-pattern Hairline border around the wrapper. Mono usage is textbook (path mono, sizes/dates plain). `link-error` for the delete action is the right primary-tone-of-danger.

The weak spots: (1) empty state is a single sentence and wastes a teaching moment; (2) delete is styled as a link but is actually a destructive `<button type="submit">` — semantic mismatch for screen readers; (3) no scale affordances (filter/sort/totals).

**Deterministic scan.** Clean. Exit 0, no findings.

#### Priority Issues

- **[P1] No filter, no sort, no totals.** The forensics use-case is "find this one file out of 200" — none of the table primitives that enable that are present. **Fix:** add a single `<input type="search">` above the table that filters rows by path substring (client-side, matches the apps.html and admin_users.html patterns); show a `<tfoot>` with file count + total size. Keep header column-widths.
  **Command:** `/impeccable layout internal/server/templates/files.html`

- **[P2] Empty state is a single sentence.** Wasted teaching surface. **Fix:** under "No files yet", list the three ways files arrive — agent build, manual upload, MCP — each with the relevant link.
  **Command:** `/impeccable onboard internal/server/templates/files.html`

- **[P2] Delete button styled as a link.** `link link-error` on a `<button type="submit">` reads as a navigation anchor to AT. **Fix:** either `class="btn btn-ghost btn-xs text-error"` (still subtle, still in-row, but reads as button) or add `aria-label="Delete {{ .Path }}"`. Also pass `data-confirm-slug="{{ .Path }}"` so the typed-confirmation ceremony in `confirm_dialog` engages — destructive + irreversible + may break homepage clears that bar.
  **Command:** `/impeccable harden internal/server/templates/files.html`

#### Persona Red Flags

- **Alex (super-admin):** no keyboard filter — Alex wants `/` to focus search.
- **Sam (a11y):** no `<caption>`, no `scope="col"`, delete-styled-as-link.

#### Minor Observations

- Lowercase column heads are a stylistic choice; keep them, but then `<strong>edit</strong>` / `<strong>open</strong>` in the prose paragraph are louder than the heads.
- `rounded-box border border-base-300` around the table is correct Hairline.
- `Modified` column has fixed `w-44` — may clip relative-time strings in non-English locales.
