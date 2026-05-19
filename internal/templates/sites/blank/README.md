# Blank canvas

## Purpose
The fallback template, picked when the user wants to describe a site that doesn't match any of the other categories. Also acts as the safety net: `templates.Get(id)` returns this when an unknown id is requested, so a stale form value never breaks a build.

## What ships
Nothing — no skeleton, no checks, no prompt addendum.

## Gotchas
The loader treats `blank` as the `defaultID` ([internal/templates/templates.go](../../templates.go)). If you rename or remove the directory, the loader's `init()` will panic on startup. Keep it as `blank`.
