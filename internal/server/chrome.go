package server

// Chrome carries the five fields the shared brand + site_subnav partials
// need on every page-data struct. Embedded anonymously so its fields
// promote into the outer struct and html/template can see them via the
// usual `.SiteName`, `.IsSuperAdmin`, etc. accessors.
//
// Per-page fields (SiteName, Slug, SiteURL, Active) stay handler-set —
// each handler knows what URL it's serving. IsSuperAdmin is the only
// session-derived field, populated by render() via the chromed interface
// below.
type Chrome struct {
	// SiteName + Slug + SiteURL are per-site: only the per-app pages
	// (workspace, manage, files, debug, function_edit) populate them.
	// Global pages (apps, admin_users, system, account, landing) leave
	// them empty and the brand partial's `{{ if .SiteName }}` skips the
	// breadcrumb.
	SiteName string
	Slug     string
	SiteURL  string

	// Active is the nav-highlight key. Compared against the literal
	// strings in the brand partial ("admin_users", "system", "account",
	// "workspace", "manage") to decide which tab gets `btn-active` /
	// `tab-active`.
	Active string

	// IsSuperAdmin gates the "Users" nav link. Populated by render()
	// from the session role; handlers should NOT set it themselves —
	// any value they pass gets overwritten.
	IsSuperAdmin bool
}

// chromePtr exposes the embedded Chrome for in-place mutation. Defined
// on *Chrome rather than the outer struct so any struct that embeds
// Chrome (by value) automatically satisfies the chromed interface when
// addressed via pointer — no per-page boilerplate needed.
func (c *Chrome) chromePtr() *Chrome { return c }

// chromed is satisfied by any *struct{ Chrome; ... } via Go's anonymous
// field-method promotion. render() uses it to inject session-derived
// chrome values without reflection.
type chromed interface {
	chromePtr() *Chrome
}
