package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jtarchie/mailcatch/mailparse"
	"github.com/jtarchie/mailcatch/mailstore"
	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/build"
)

// mailController is the per-site caught-mail surface: owner + collaborators read it (canAccess via `owns`), only a super admin turns it on.
type mailController struct{ *Server }

func (s *mailController) register(g *echo.Group, owns echo.MiddlewareFunc) {
	g.GET("/mail/:slug", s.listHandler, owns)
	g.POST("/mail/:slug/settings", s.settingsHandler, owns)
	g.POST("/mail/:slug/clear", s.clearHandler, owns)
	g.GET("/mail/:slug/m/:inbox/:id", s.messageHandler, owns)
	g.GET("/mail/:slug/m/:inbox/:id/html", s.htmlHandler, owns)
	g.GET("/mail/:slug/m/:inbox/:id/raw", s.rawHandler, owns)
	g.POST("/mail/:slug/m/:inbox/:id/delete", s.deleteHandler, owns)
}

// mailHTMLCSP sandboxes mail HTML: no scripts, forms, remote fetches (tracking pixels) or same-origin access to the admin app.
const mailHTMLCSP = "sandbox allow-popups allow-popups-to-escape-sandbox; default-src 'none'; img-src data:; " +
	"style-src 'unsafe-inline'; font-src data:; frame-ancestors 'self'"

const mailListLimit = 200

type mailTTLChoice struct {
	Hours int
	Label string
}

// mailTTLChoices are the retention options the owner picks from; a closed list keeps a forged form from setting years.
var mailTTLChoices = []mailTTLChoice{{1, "1 hour"}, {24, "1 day"}, {24 * 7, "7 days"}, {24 * 30, "30 days"}}

type mailData struct {
	Chrome
	Enabled   bool
	TTLHours  int
	TTLs      []mailTTLChoice
	Addresses []string
	MXHost    string
	Inbox     string
	Inboxes   []string
	Messages  []mailstore.Message
	Message   *mailstore.Message
	Body      mailparse.Body
	Flash     string
}

func (s *mailController) chrome(c *echo.Context, slug string, meta build.SiteMeta) mailData {
	addrs := make([]string, 0, 1+len(meta.Domains))
	addrs = append(addrs, "anything@"+slug+"."+s.domain)
	for _, d := range meta.Domains {
		addrs = append(addrs, "anything@"+d)
	}
	ttl := int(meta.MailTTL().Hours())
	return mailData{
		Chrome:    (&sitesController{s.Server}).siteChrome(c, slug, meta.Title, "mail"),
		Enabled:   meta.MailEnabled,
		TTLHours:  ttl,
		TTLs:      mailTTLChoices,
		Addresses: addrs,
		MXHost:    s.mailHost,
		Flash:     c.QueryParam("flash"),
	}
}

func (s *mailController) listHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	data := s.chrome(c, slug, s.build.ReadMeta(ctx, slug))

	all, err := s.mail.List(ctx, slug, "", mailListLimit)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list mail", err)
	}
	seen := map[string]bool{}
	for _, m := range all {
		if !seen[m.Inbox] {
			seen[m.Inbox] = true
			data.Inboxes = append(data.Inboxes, m.Inbox)
		}
	}
	data.Messages = all
	if want, ok := mailstore.NormalizeInbox(c.QueryParam("inbox")); ok {
		data.Inbox = want
		data.Messages = data.Messages[:0:0]
		for _, m := range all {
			if m.Inbox == want {
				data.Messages = append(data.Messages, m)
			}
		}
	}
	return s.render(c, "mail", data)
}

// lookup fetches one message keyed by slug, inbox and id together; any mismatch is a 404, never a hint that the id exists elsewhere.
func (s *mailController) lookup(c *echo.Context) (string, mailstore.Message, error) {
	slug, err := slugParam(c)
	if err != nil {
		return "", mailstore.Message{}, err
	}
	inbox, ok := mailstore.NormalizeInbox(c.Param("inbox"))
	if !ok {
		return "", mailstore.Message{}, notFound()
	}
	m, err := s.mail.Get(c.Request().Context(), slug, inbox, c.Param("id"))
	if errors.Is(err, mailstore.ErrNotFound) {
		return "", mailstore.Message{}, notFound()
	}
	if err != nil {
		return "", mailstore.Message{}, httpErr(http.StatusInternalServerError, "read mail", err)
	}
	return slug, m, nil
}

func (s *mailController) messageHandler(c *echo.Context) error {
	slug, m, err := s.lookup(c)
	if err != nil {
		return err
	}
	data := s.chrome(c, slug, s.build.ReadMeta(c.Request().Context(), slug))
	data.Message = &m
	data.Body = mailparse.ParseBody(m.Raw)
	return s.render(c, "mail", data)
}

// htmlHandler is only ever framed by the message page; <base target=_blank> makes links leave the sandbox as new tabs.
func (s *mailController) htmlHandler(c *echo.Context) error {
	_, m, err := s.lookup(c)
	if err != nil {
		return err
	}
	h := c.Response().Header()
	h.Set("Content-Security-Policy", mailHTMLCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	return c.HTML(http.StatusOK, `<!doctype html><meta charset="utf-8"><base target="_blank">`+mailparse.ParseBody(m.Raw).HTML) //nolint:wrapcheck // echo renders its own errors
}

func (s *mailController) rawHandler(c *echo.Context) error {
	_, m, err := s.lookup(c)
	if err != nil {
		return err
	}
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", m.Raw) //nolint:wrapcheck // echo renders its own errors
}

func (s *mailController) deleteHandler(c *echo.Context) error {
	slug, m, err := s.lookup(c)
	if err != nil {
		return err
	}
	err = s.mail.Delete(c.Request().Context(), slug, m.Inbox, m.ID)
	if err != nil && !errors.Is(err, mailstore.ErrNotFound) {
		return httpErr(http.StatusInternalServerError, "delete mail", err)
	}
	return c.Redirect(http.StatusSeeOther, "/mail/"+slug+"?flash="+url.QueryEscape("Message deleted.")) //nolint:wrapcheck // echo renders its own errors
}

func (s *mailController) clearHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	n, err := s.mail.DeleteTenant(c.Request().Context(), slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "clear mail", err)
	}
	return c.Redirect(http.StatusSeeOther, "/mail/"+slug+"?flash="+url.QueryEscape("Deleted "+strconv.Itoa(n)+" messages.")) //nolint:wrapcheck // echo renders its own errors
}

// settingsHandler lets anyone with access pick retention, but only a super admin flips MailEnabled: it opens our MX to the world for this site.
func (s *mailController) settingsHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	hours, err := strconv.Atoi(c.FormValue("ttl_hours"))
	if err != nil || !validMailTTL(hours) {
		return httpErr(http.StatusBadRequest, "pick one of the listed retention periods", err)
	}
	u := userFromContext(c)
	super := u != nil && u.Role == auth.RoleSuperAdmin
	enable := strings.EqualFold(c.FormValue("enabled"), "on")

	ctx := c.Request().Context()
	_, err = s.build.UpdateMeta(ctx, slug, func(m *build.SiteMeta) {
		m.MailTTLHours = hours
		if super {
			m.MailEnabled = enable
		}
	})
	if err != nil {
		return httpErr(http.StatusInternalServerError, "save mail settings", err)
	}
	// The mail index is what the SMTP resolver reads; without a rebuild a toggle wouldn't take effect until the next unrelated save.
	s.registry.rebuildIndexesLogging(ctx)
	return c.Redirect(http.StatusSeeOther, "/mail/"+slug+"?flash="+url.QueryEscape("Mail settings saved.")) //nolint:wrapcheck // echo renders its own errors
}

func validMailTTL(hours int) bool {
	for _, t := range mailTTLChoices {
		if t.Hours == hours {
			return true
		}
	}
	return false
}
