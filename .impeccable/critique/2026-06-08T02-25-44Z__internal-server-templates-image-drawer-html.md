---
target: managing images for a site
total_score: 24
p0_count: 0
p1_count: 3
timestamp: 2026-06-08T02-25-44Z
slug: internal-server-templates-image-drawer-html
---
## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | No `aria-live` on status pill; no toast after Insert ("dropped into prompt") |
| 2 | Match System / Real World | 3 | Drawer pattern is familiar; n/a |
| 3 | User Control and Freedom | 2 | No delete from drawer; Insert silently discards unsaved alt edits; Esc from detail closes the whole drawer instead of going back to the grid |
| 4 | Consistency and Standards | 2 | Save + Insert compete in the same toolbar; "link link-hover" used for buttons (Browse, Upload one.); `shadow-lg` violates DESIGN.md's flat-surface rule |
| 5 | Error Prevention | 2 | Insert without Save discards typed edits with no warning; no confirm for upload-overwrite of same filename |
| 6 | Recognition Rather Than Recall | 3 | Thumbnails, filename, alt all visible at once; nothing hidden |
| 7 | Flexibility and Efficiency | 2 | No drag-and-drop, no keyboard arrows across the grid, no bulk select, no search/filter |
| 8 | Aesthetic and Minimalist Design | 3 | Clean drawer, no decoration; status pill placement in header is awkward |
| 9 | Error Recovery | 2 | "Save failed: HTTP 413" leaks transport detail; "1 upload failed" doesn't name the file |
| 10 | Help and Documentation | 2 | Placeholders explain fields, but no inline "what is this for" for the agent-reference use case |
| **Total** | | **24/40** | **Acceptable — needs work before "feels good"** |

## Anti-Patterns Verdict

The drawer is conservative and on-brand for product UI — single typeface, daisyUI primitives, flat composition. No AI-slop tells. The two real problems are "two-buttons-one-toolbar" and `shadow-lg` on a brand that's flat-by-default. Both are pre-existing project tells (Themes/History panels already use shadow-lg) more than drawer-specific.

Detector found 7 false positives in scope: dynamic-src `<img>` is populated on card click; Inter is a committed brand decision; flat type hierarchy hits are on host pages, not the drawer.

## Overall Impression

The drawer makes the right structural moves — one component, three modes, real PATCH endpoint, real Insert hook. What undermines it is the detail view: "Save" and "Insert" sit shoulder-to-shoulder and clicking Insert silently abandons unsaved metadata edits. Second biggest opportunity: delete lives elsewhere (`/files/:slug`), so the drawer's "manage" promise breaks.

## What's Working

- One artifact, three contexts (workspace text insert, visual-editor GrapesJS insert, manage view-only) via a clean mode-based onInsert callback.
- Server trims/caps; the UI reflects the server's value back into the inputs after Save.
- Empty state has a verb: "No images yet. Upload one."
- Reuses the .side-panel CSS so motion matches Themes/History.

## Priority Issues

### [P1] Save and Insert compete; Insert silently loses edits
Field-level autosave on blur would eliminate the collision. Drop the Save button; Insert becomes the only commit verb. Suggested command: /impeccable clarify.

### [P1] Delete is missing from the drawer entirely
Add a Delete action to the detail view (btn-error btn-sm btn-outline). Wire to a new DELETE /assets/:slug/* or reuse POST /files/:slug/delete. Use the existing js-confirm dialog. Suggested command: /impeccable harden.

### [P1] "Reference image…" copy is technical and ambiguous
Rename to "Pick image…". After insert, toast: "Added `assets/hero.png` to your prompt — describe how to use it." Convert "Browse" link to btn-ghost btn-xs. Suggested command: /impeccable clarify.

### [P2] Status messages overload one slot, no aria-live, no copy hierarchy
Add aria-live="polite". Differentiate success styling. Move empty-state copy out of the status pill into the grid region. Suggested command: /impeccable polish.

### [P2] Insert produces a brittle string instead of a structured reference
Render picked images as removable chips above the prompt; hidden multi-value form field. Mirrors the existing selection-chip pattern (workspace.html lines 138-142). Suggested command: /impeccable shape.

## Persona Red Flags

**Alex (Power User)**: No drag-and-drop, no keyboard arrows in the grid, no bulk select, no delete, no search. Tolerable but flagged as "junior tool."

**Jordan (First-Timer)**: Save and Insert both read as "commit." Hits Save, sees "Saved", drawer doesn't close, confused. Hits Insert, drawer closes, sees `assets/foo.png` in the prompt, doesn't know what to do next.

**Sam (Accessibility-Dependent User)**: No aria-live on status pill. Esc from detail closes whole drawer (regression). Card buttons get accessible name twice (text + img alt). Focus doesn't move to the detail region on grid→detail transition.

**Riley (Stress Tester)**: "1 upload failed" no filename, no reason. 50 thumbnails render synchronously (no virtualization). Alt truncation at 125 is silent. Two-tab editing leaves the second tab stale until refresh.

## Minor Observations

- shadow-lg on the panel contradicts DESIGN.md flat-surface rule (pre-existing across all side panels).
- style="display:none" inline in image_drawer.html line 22 — use Tailwind's hidden class.
- Grid hard-coded to 2 cols; if the panel ever widens, switch to grid-cols-[repeat(auto-fit,minmax(160px,1fr))].
- "no alt" italic placeholder uses an extra span; can be plain italic.
- escapeHTML is hand-built rather than DOM-constructed; low risk because data is same-origin.
- workspace.html has #drop-zone that's never wired; drawer also doesn't accept drops.

## Questions to Consider

- What if the detail view had no Save button at all (autosave on blur)?
- Should delete live in a hover-action on the card itself? Hover hides for Sam, though.
- "View" mode on manage and "Browse" mode on workspace are identical except for the Insert button — could be one mode.
- Should picking an image also offer to drop the <img src> directly into the current page?
