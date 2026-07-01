package server

import (
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/guide"
	"github.com/jtarchie/topbanana/internal/templates"
)

// manageSubmissionLimit caps how many submission rows we render inline on
// /manage/:slug. Beyond this, the page shows a "+ N more" note and the CSV
// download is the path to the full set. Pagination would add clicks and
// state to a screen most users skim; CSV in a spreadsheet is the better
// tool for bulk analysis anyway.
const manageSubmissionLimit = 25

// manageData is the single struct backing the consolidated /manage/:slug page.
// It carries everything that was previously split across the settings page and
// the form-submissions page so the user sees one config surface, not three.
type manageData struct {
	Chrome
	Title            string
	Domains          string
	FunctionsEnabled bool
	FunctionsByTmpl  bool
	PublicAPIEnabled bool
	Private          bool
	Columns          []string
	Rows             []dataRow // capped at manageSubmissionLimit
	// TotalRows is the unsliced count so the template can render
	// "+ N more, download CSV for all".
	TotalRows int
	// MoreCount is TotalRows - len(Rows), exposed pre-computed because
	// html/template has no arithmetic helpers.
	MoreCount int
	CSVURL    string
	JSONURL   string
	Flash     string
	// TemplateLabel + SetupNotes surface end-user "you picked this template,
	// here's what to set up" guidance. Notes are pre-rendered to HTML in the
	// handler so the manage template can drop them in without escaping logic.
	TemplateLabel string
	SetupNotes    template.HTML
	// DNSCNAMETarget is the hostname a custom-domain CNAME record should point
	// at — the configured CustomDomainCNAME, or the base domain when unset.
	// Rendered verbatim into the copy-paste DNS instructions.
	DNSCNAMETarget string
	// Guide* back the "Is my site complete?" card — a deterministic, per-type
	// content checklist (internal/guide). GuideTotal == 0 hides the card (a
	// template that declares no guide).
	GuideResults  []guide.Result
	GuidePresent  int
	GuideTotal    int
	GuideComplete bool
	// PhotoWall gates the event-photo-wall summary card. When true, PendingPhotos
	// / ApprovedPhotos hold the queue counts and PhotoQueueURL links the owner to
	// the moderation queue.
	PhotoWall      bool
	PendingPhotos  int
	ApprovedPhotos int
	PhotoQueueURL  string
}

// urlPattern matches bare http/https URLs anywhere in setup-notes text. Kept
// liberal (no scheme other than http(s); no IDN niceties) because the input is
// authored by template maintainers, not untrusted users.
var urlPattern = regexp.MustCompile(`https?://[^\s<>"')]+`)

// renderSetupNotes converts plain-text setup notes into a small bundle of
// <p>...</p> blocks with bare URLs auto-linked. Blank lines split paragraphs;
// single newlines within a paragraph become <br>. Everything else is
// HTML-escaped. Returns "" when there's nothing to render so the caller can
// branch on truthiness in the template.
func renderSetupNotes(notes string) template.HTML {
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return ""
	}
	var b strings.Builder
	for _, para := range strings.Split(notes, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		b.WriteString("<p>")
		for i, line := range strings.Split(para, "\n") {
			if i > 0 {
				b.WriteString("<br>")
			}
			cursor := 0
			for _, match := range urlPattern.FindAllStringIndex(line, -1) {
				b.WriteString(template.HTMLEscapeString(line[cursor:match[0]]))
				url := line[match[0]:match[1]]
				b.WriteString(`<a href="`)
				b.WriteString(template.HTMLEscapeString(url))
				b.WriteString(`" target="_blank" rel="noopener" class="link link-primary">`)
				b.WriteString(template.HTMLEscapeString(url))
				b.WriteString(`</a>`)
				cursor = match[1]
			}
			b.WriteString(template.HTMLEscapeString(line[cursor:]))
		}
		b.WriteString("</p>")
	}
	// Every interpolated rune above went through template.HTMLEscapeString;
	// the surrounding tags are literals. template.HTML is safe here.
	return template.HTML(b.String()) //nolint:gosec // G203: see comment.
}

func (s *sitesController) manageHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}

	ctx := c.Request().Context()
	meta := s.build.ReadMeta(ctx, slug)
	base := templates.Get(meta.Template)
	byTmpl := base != nil && base.EnablesFunctions
	tmpl := build.EffectiveTemplate(meta)

	var tmplLabel string
	var setupNotes template.HTML
	if base != nil {
		tmplLabel = base.Label
		setupNotes = renderSetupNotes(base.SetupNotes)
	}

	// Deterministic per-type completeness checklist. Pass the base (type)
	// template, not the functions-override EffectiveTemplate — guide items
	// describe the site type, not its runtime capabilities.
	report := guide.Evaluate(ctx, s.store, slug, base)

	cols, rows, err := s.collectSubmissions(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "load submissions", err)
	}
	total := len(rows)
	// Slice cap is bounded by the if: total = len(rows), so
	// total > manageSubmissionLimit implies len(rows) > manageSubmissionLimit
	// and rows can't be nil at that point.
	if total > manageSubmissionLimit {
		rows = rows[:manageSubmissionLimit] //nolint:nilaway // see comment.
	}
	more := total - len(rows)

	siteName := meta.Title
	if siteName == "" {
		siteName = slug
	}

	cnameTarget := s.systemInfo.CustomDomainCNAME
	if cnameTarget == "" {
		cnameTarget = s.domain
	}

	// Event-photo-wall summary. base==nil falls back to no wall; the counts come
	// from the same state blob as submissions, tallied once.
	photoWall := base != nil && base.EnablesPhotoWall
	pendingPhotos, approvedPhotos := 0, 0
	if photoWall {
		pending, approved := s.photoCounts(ctx, slug)
		pendingPhotos, approvedPhotos = len(pending), approved
	}

	return s.render(c, "manage", manageData{
		Chrome: Chrome{
			Slug:     slug,
			SiteName: siteName,
			SiteURL:  s.siteURL(c, slug, "/"),
			Active:   "manage",
		},
		Title:            meta.Title,
		Domains:          strings.Join(meta.Domains, "\n"),
		FunctionsEnabled: tmpl != nil && tmpl.EnablesFunctions,
		FunctionsByTmpl:  byTmpl,
		PublicAPIEnabled: meta.EnablesPublicAPI,
		Private:          meta.Private,
		Columns:          cols,
		Rows:             rows,
		TotalRows:        total,
		MoreCount:        more,
		CSVURL:           "/data/" + slug + "?format=csv",
		JSONURL:          "/data/" + slug + "?format=json",
		Flash:            c.QueryParam("flash"),
		TemplateLabel:    tmplLabel,
		SetupNotes:       setupNotes,
		DNSCNAMETarget:   cnameTarget,
		GuideResults:     report.Results,
		GuidePresent:     report.Present,
		GuideTotal:       report.Total,
		GuideComplete:    report.Complete(),
		PhotoWall:        photoWall,
		PendingPhotos:    pendingPhotos,
		ApprovedPhotos:   approvedPhotos,
		PhotoQueueURL:    "/photos/" + slug,
	})
}
