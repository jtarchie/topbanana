package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/mailcatch/mailstore"
	"github.com/jtarchie/mailcatch/smtpd"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/mail"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/storetest"
)

type mailRig struct {
	*privateTestRig
	srv  *server.Server
	mail *mail.Store
	base string
}

func newMailRig(t *testing.T, slug, owner string, meta build.SiteMeta) *mailRig {
	t.Helper()
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	mustWrite(t, ctx, st, slug, "index.html", "<h1>hi</h1>", "text/html")
	meta.Template, meta.OwnerID = "blank", owner
	writeMeta(t, ctx, st, slug, meta)

	base := newPrivateRig(t, st, snapSvc)
	ms := mail.New(st, mail.Options{Prefix: store.MailPrefix + storetest.FreshSlug(t, "mailtest") + "/"})
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	e, srv := server.New(server.Deps{
		Store: st, Build: build.New(st, nil, tracker, snapSvc), Events: tracker, State: state.NewMemory(),
		Snapshot: snapSvc, Auth: base.auth, Domain: "localhost", Port: "8080", Mail: ms,
	})
	httpSrv := httptest.NewServer(e)
	t.Cleanup(httpSrv.Close)
	return &mailRig{privateTestRig: &privateTestRig{handler: e, auth: base.auth, store: st}, srv: srv, mail: ms, base: httpSrv.URL}
}

// get returns status, headers and body with the response already closed, so callers can't leak it.
func (r *mailRig) get(t *testing.T, path string, cookie *http.Cookie) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header, string(body)
}

// TestMail_EnableIsSuperAdminOnly is the abuse gate: an owner can set retention but never open the MX for their own site.
func TestMail_EnableIsSuperAdminOnly(t *testing.T) {
	const owner = "mail-owner@test"
	slug := "mail-" + freshSlug(t)
	r := newMailRig(t, slug, owner, build.SiteMeta{Domains: []string{"mail-custom.test"}})
	ownerCookie := r.session(t, owner, auth.RoleAdmin)
	superCookie := r.session(t, testAdminUser, auth.RoleSuperAdmin)

	if _, _, ok := r.srv.MailRoute(slug + ".localhost"); ok {
		t.Fatal("disabled site routed mail")
	}

	resp := postForm(t, r.base, "/mail/"+slug+"/settings", ownerCookie, url.Values{"enabled": {"on"}, "ttl_hours": {"24"}})
	_ = resp.Body.Close()
	meta := readMetaDirect(t, r.privateTestRig, slug)
	if resp.StatusCode != http.StatusSeeOther || meta.MailEnabled || meta.MailTTLHours != 24 {
		t.Fatalf("owner save: %d enabled=%v ttl=%d; want 303, still disabled, ttl 24", resp.StatusCode, meta.MailEnabled, meta.MailTTLHours)
	}

	resp = postForm(t, r.base, "/mail/"+slug+"/settings", superCookie, url.Values{"enabled": {"on"}, "ttl_hours": {"9999"}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("off-list ttl: got %d want 400", resp.StatusCode)
	}

	resp = postForm(t, r.base, "/mail/"+slug+"/settings", superCookie, url.Values{"enabled": {"on"}, "ttl_hours": {"168"}})
	_ = resp.Body.Close()
	if !readMetaDirect(t, r.privateTestRig, slug).MailEnabled {
		t.Fatal("super admin could not enable mail")
	}

	for host, want := range map[string]bool{
		slug + ".localhost":        true,
		"MAIL-CUSTOM.test.":        true,
		"x." + slug + ".localhost": false,
		"nosuch-site.localhost":    false,
		"localhost":                false,
		"unrelated.example":        false,
	} {
		got, ttl, ok := r.srv.MailRoute(host)
		if ok != want || (ok && (got != slug || ttl != 168*time.Hour)) {
			t.Errorf("MailRoute(%q) = %q %v %v, want routed=%v", host, got, ttl, ok, want)
		}
	}
}

// TestMail_ReadAccessAndSandbox covers who can read caught mail and how its HTML is served.
func TestMail_ReadAccessAndSandbox(t *testing.T) {
	const owner = "mail-reader@test"
	slug := "mailr-" + freshSlug(t)
	r := newMailRig(t, slug, owner, build.SiteMeta{MailEnabled: true})
	ownerCookie := r.session(t, owner, auth.RoleAdmin)
	strangerCookie := r.session(t, "mail-stranger@test", auth.RoleAdmin)

	raw := "Subject: Welcome aboard\r\nContent-Type: text/html\r\n\r\n<p>hi</p><script>alert(1)</script>"
	err := r.mail.Deliver(context.Background(), []mailstore.Message{{
		Tenant: slug, Inbox: "signup", Domain: slug + ".localhost", ReceivedAt: time.Now(),
		From: "app@sender.test", Subject: "Welcome aboard", Raw: []byte(raw),
	}})
	if err != nil {
		t.Fatal(err)
	}
	list, err := r.mail.List(context.Background(), slug, "", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	msgPath := "/mail/" + slug + "/m/signup/" + list[0].ID

	status, _, body := r.get(t, "/mail/"+slug, ownerCookie)
	if status != http.StatusOK || !strings.Contains(body, "Welcome aboard") {
		t.Fatalf("owner list: %d", status)
	}
	for _, p := range []string{"/mail/" + slug, msgPath, msgPath + "/html", msgPath + "/raw", "/mail/" + slug + "/m/other/" + list[0].ID} {
		cookie := strangerCookie
		if strings.Contains(p, "/m/other/") {
			cookie = ownerCookie // the owner, but the id under the wrong inbox
		}
		if status, _, _ := r.get(t, p, cookie); status != http.StatusNotFound {
			t.Errorf("%s: got %d want 404", p, status)
		}
	}

	status, _, body = r.get(t, msgPath, ownerCookie)
	if status != http.StatusOK || !strings.Contains(body, `sandbox="allow-popups allow-popups-to-escape-sandbox"`) || !strings.Contains(body, "signup@"+slug) {
		t.Fatalf("owner message view: %d", status)
	}

	assertMailSandboxed(t, r, msgPath, ownerCookie)
	assertDeleteCascadesMail(t, r, slug, ownerCookie)
}

// assertMailSandboxed: mail HTML is attacker-authored, so it must never run script or touch the admin origin.
func assertMailSandboxed(t *testing.T, r *mailRig, msgPath string, ownerCookie *http.Cookie) {
	t.Helper()
	_, h, _ := r.get(t, msgPath+"/html", ownerCookie)
	csp := h.Get("Content-Security-Policy")
	if !strings.HasPrefix(csp, "sandbox ") || strings.Contains(csp, "allow-scripts") || strings.Contains(csp, "allow-same-origin") {
		t.Errorf("mail HTML CSP = %q; want sandbox without scripts or same-origin", csp)
	}
	_, h, body := r.get(t, msgPath+"/raw", ownerCookie)
	if !strings.HasPrefix(h.Get("Content-Type"), "text/plain") || !strings.Contains(body, "<script>") {
		t.Errorf("raw: %q", h.Get("Content-Type"))
	}
}

// assertDeleteCascadesMail: mail sits outside the slug prefix, so deleting the site has to reach it explicitly.
func assertDeleteCascadesMail(t *testing.T, r *mailRig, slug string, ownerCookie *http.Cookie) {
	t.Helper()
	resp := postForm(t, r.base, "/apps/"+slug, ownerCookie, url.Values{"_method": {"DELETE"}, "confirm": {slug}})
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("delete site: %d", resp.StatusCode)
	}
	if left, _ := r.mail.List(context.Background(), slug, "", 10); len(left) != 0 {
		t.Fatalf("mail survived site deletion: %d left", len(left))
	}
}

// TestMCP_MailTools covers the agent's verify-and-clean-up loop and that another user's slug stays invisible.
func TestMCP_MailTools(t *testing.T) {
	st := minioStore(t)
	slug := freshSlug(t)
	const owner = "mail-mcp@example.com"
	ms := mail.New(st, mail.Options{Prefix: store.MailPrefix + storetest.FreshSlug(t, "mailmcp") + "/"})
	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner, func(d *server.Deps) { d.Mail = ms })
	seedUser(t, authBlobs, owner)
	seedUser(t, authBlobs, "mail-mcp-other@example.com")
	session := connectMCP(t, srv, owner)
	other := connectMCP(t, srv, "mail-mcp-other@example.com")

	err := ms.Deliver(context.Background(), []mailstore.Message{{
		Tenant: slug, Inbox: "hello", Domain: slug + ".localhost", ReceivedAt: time.Now(),
		From: "x@sender.test", Subject: "Confirm your email", Raw: []byte("Subject: Confirm your email\r\n\r\nclick https://x.test/c"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := ms.List(context.Background(), slug, "", 1)

	if res := callTool(t, other, "list_mail", map[string]any{"slug": slug}); !res.IsError {
		t.Fatalf("another user listed mail: %s", toolText(res))
	}
	res := callTool(t, session, "read_mail", map[string]any{"slug": slug, "inbox": "hello", "id": list[0].ID})
	if res.IsError || !strings.Contains(toolText(res), "https://x.test/c") {
		t.Fatalf("read_mail: %s", toolText(res))
	}
	if res := callTool(t, session, "delete_mail", map[string]any{"slug": slug, "inbox": "hello", "id": list[0].ID}); res.IsError {
		t.Fatalf("delete_mail: %s", toolText(res))
	}
	if left, _ := ms.List(context.Background(), slug, "", 1); len(left) != 0 {
		t.Fatal("delete_mail left the message")
	}
}

// TestMail_SMTPEndToEnd drives a real SMTP session through mailcatch's server into the S3 driver, with topbanana's resolver deciding routing.
func TestMail_SMTPEndToEnd(t *testing.T) {
	slug := "mailsmtp-" + freshSlug(t)
	r := newMailRig(t, slug, "mail-smtp@test", build.SiteMeta{MailEnabled: true, MailTTLHours: 1})

	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	smtpSrv := smtpd.NewGoSMTP(smtpd.Config{
		Hostname: "mx.localhost",
		Resolve: func(domain string) (smtpd.Route, bool) {
			s, ttl, ok := r.srv.MailRoute(domain)
			return smtpd.Route{Tenant: s, TTL: ttl}, ok
		},
	}, r.mail)
	go func() { _ = smtpSrv.Serve(l) }()
	t.Cleanup(func() { _ = smtpSrv.Shutdown(context.Background()) })

	c, err := smtp.Dial(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	err = c.Mail("sender@elsewhere.test")
	if err != nil {
		t.Fatal(err)
	}
	rerr := c.Rcpt("someone@disabled-site.localhost")
	if rerr == nil || !strings.HasPrefix(rerr.Error(), "550") {
		t.Fatalf("unknown host: %v, want 550 at RCPT", rerr)
	}
	err = c.Rcpt("Hello+tag@" + slug + ".localhost")
	if err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "From: App <app@elsewhere.test>\r\nSubject: Over the wire\r\n\r\nhi\r\n")
	err = w.Close()
	if err != nil {
		t.Fatal(err)
	}

	list, err := r.mail.List(context.Background(), slug, "hello", 5)
	if err != nil || len(list) != 1 || list[0].Subject != "Over the wire" {
		t.Fatalf("delivered: %+v %v", list, err)
	}
	if ttl := list[0].ExpiresAt.Sub(list[0].ReceivedAt).Round(time.Second); ttl != time.Hour {
		t.Errorf("site TTL not applied: %v", ttl)
	}
}

// TestMCP_ConfigureSiteMail: the MCP toggle keeps the web rule, super admins only.
func TestMCP_ConfigureSiteMail(t *testing.T) {
	st := minioStore(t)
	slug := freshSlug(t)
	const owner = "mail-cfg@example.com"
	const super = "mail-cfg-super@example.com"
	ms := mail.New(st, mail.Options{Prefix: store.MailPrefix + storetest.FreshSlug(t, "mailcfg") + "/"})
	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner, func(d *server.Deps) { d.Mail = ms })
	seedUser(t, authBlobs, owner)
	seedSuperAdmin(t, authBlobs, super)
	ownerS := connectMCP(t, srv, owner)
	superS := connectMCP(t, srv, super)

	res := callTool(t, ownerS, "configure_site", map[string]any{"slug": slug, "mail_enabled": true})
	if !res.IsError {
		t.Fatalf("owner enabled mail: %s", toolText(res))
	}
	res = callTool(t, ownerS, "configure_site", map[string]any{"slug": slug, "mail_ttl_hours": 24})
	if res.IsError || !strings.Contains(toolText(res), `"mail_ttl_hours":24`) {
		t.Fatalf("owner ttl: %s", toolText(res))
	}
	res = callTool(t, superS, "configure_site", map[string]any{"slug": slug, "mail_enabled": true})
	if res.IsError || !strings.Contains(toolText(res), `"mail_enabled":true`) {
		t.Fatalf("super admin enable: %s", toolText(res))
	}
	res = callTool(t, ownerS, "list_mail", map[string]any{"slug": slug})
	if !strings.Contains(toolText(res), `"enabled":true`) {
		t.Fatalf("index not refreshed: %s", toolText(res))
	}
}

// TestMail_RestoreKeepsSuperAdminGrant: a snapshot from while mail was on must not let an owner re-open mail a super admin shut off.
func TestMail_RestoreKeepsSuperAdminGrant(t *testing.T) {
	const owner = "mail-restore@test"
	slug := "mailrs-" + freshSlug(t)
	r := newMailRig(t, slug, owner, build.SiteMeta{MailEnabled: true})
	ownerCookie := r.session(t, owner, auth.RoleAdmin)
	superCookie := r.session(t, testAdminUser, auth.RoleSuperAdmin)

	snap, err := snapshot.New(r.store, 0).Create(context.Background(), slug, snapshot.ReasonSettings)
	if err != nil {
		t.Fatal(err)
	}
	resp := postForm(t, r.base, "/mail/"+slug+"/settings", superCookie, url.Values{"ttl_hours": {"168"}})
	_ = resp.Body.Close()
	if _, _, on := r.srv.MailRoute(slug + ".localhost"); on {
		t.Fatal("super admin could not disable mail")
	}

	resp = postForm(t, r.base, "/history/"+slug, ownerCookie, url.Values{"_method": {"PUT"}, "key": {snap.Key}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("owner restore: got %d want 303", resp.StatusCode)
	}
	if readMetaDirect(t, r.privateTestRig, slug).MailEnabled {
		t.Error("restore brought back mail_enabled from the snapshot")
	}
	if _, _, on := r.srv.MailRoute(slug + ".localhost"); on {
		t.Error("restored snapshot re-opened mail routing")
	}
}
