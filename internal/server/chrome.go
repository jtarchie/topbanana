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
	// strings in the brand + admin_subnav + site_subnav partials
	// ("apps", "account", "admin_users", "admin_clients", "system",
	// "workspace", "inbox", "manage") to decide which tab gets
	// `btn-active` / `tab-active`.
	Active string

	// IsSuperAdmin gates the "Admin" nav link and the whole operator
	// section. Populated by render() from the session role; handlers
	// should NOT set it themselves — any value they pass gets
	// overwritten.
	IsSuperAdmin bool

	// InAdmin is true on the super-admin-only operator surfaces, derived
	// from Active by render() via adminActive below. The brand partial
	// highlights its single "Admin" entry from this, so the highlight can't
	// drift out of sync with which pages the section actually contains.
	InAdmin bool

	// MCPEnabled gates the admin sub-nav's "Connections" tab: with no
	// --mcp-secret there is no OAuth client registry to show, so the
	// destination would always be empty. Injected by render() for the
	// same reason as IsSuperAdmin — every admin page needs it for the
	// shared sub-nav, and none of them should have to remember to set it.
	MCPEnabled bool

	// Year is the current calendar year, injected by render() so the
	// shared footer can render `© {{ .Year }} Top Banana` without each
	// handler threading time.Now() through its page-data struct.
	Year int
}

// adminActive is the set of Active keys that belong to the operator
// section. Single source of truth so the "Admin" nav item and the admin
// sub-nav can never disagree about which pages are in that section.
var adminActive = map[string]bool{
	"admin_users":   true,
	"admin_clients": true,
	"system":        true,
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
