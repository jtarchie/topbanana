package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/imroc/req/v3"
	"github.com/tdewolff/minify/v2"
	mincss "github.com/tdewolff/minify/v2/css"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/jtarchie/bloomhollow/internal/events"
)

type fetchReferenceArgs struct {
	URL string `json:"url"`
}

type fetchReferenceResult struct {
	Content   string `json:"content,omitempty"`
	FinalURL  string `json:"final_url,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}

const (
	fetchReferenceTimeout            = 15 * time.Second
	fetchReferenceRequestTimeout     = 5 * time.Second
	fetchReferenceMaxBytes           = maxHTMLFileBytes
	fetchReferenceMaxStylesheets     = 8
	fetchReferenceMaxStylesheetBytes = 64 * 1024
	fetchReferenceUserAgent          = "Bloomhollow/1.0 reference fetcher"
)

// blockedIPCheck is the predicate validateReferenceURL consults to reject
// private/loopback hosts. Indirected through a package var so integration
// tests can route through httptest.Server on 127.0.0.1.
var blockedIPCheck = isBlockedIP

// validateReferenceURL parses rawURL and rejects schemes other than http/https,
// the literal "localhost", and any host that resolves to a loopback, private,
// link-local, unspecified, or multicast address. The resolver is injected for
// testing; pass nil to use net.LookupIP.
func validateReferenceURL(rawURL string, resolve func(host string) ([]net.IP, error)) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q (only http and https allowed)", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, errors.New("missing host")
	}
	if strings.EqualFold(host, "localhost") {
		return nil, errors.New("localhost is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedIPCheck(ip) {
			return nil, fmt.Errorf("ip %s is in a private, loopback, or link-local range", ip)
		}
		return parsed, nil
	}
	if resolve == nil {
		resolve = net.LookupIP
	}
	ips, err := resolve(host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("host %s has no addresses", host)
	}
	for _, ip := range ips {
		if blockedIPCheck(ip) {
			return nil, fmt.Errorf("host %s resolves to blocked ip %s", host, ip)
		}
	}
	return parsed, nil
}

func isBlockedIP(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// newFetchReferenceClient builds a req client that re-validates every redirect
// target so a 302 to an internal IP can't bypass the initial SSRF guard.
func newFetchReferenceClient() *req.Client {
	return req.C().
		SetTimeout(fetchReferenceTimeout).
		SetUserAgent(fetchReferenceUserAgent).
		SetRedirectPolicy(func(r *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			_, err := validateReferenceURL(r.URL.String(), nil)
			if err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			return nil
		})
}

// fetchPage GETs target and returns the parsed body, the post-redirect URL,
// and whether the body was truncated to fit fetchReferenceMaxBytes. Non-HTML
// content types and 4xx/5xx statuses are surfaced as errors.
func fetchPage(ctx context.Context, client *req.Client, target string) ([]byte, *url.URL, bool, error) {
	resp, err := client.R().SetContext(ctx).Get(target)
	if err != nil {
		return nil, nil, false, fmt.Errorf("fetch page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, nil, false, fmt.Errorf("fetch page: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "text/html") {
		return nil, nil, false, fmt.Errorf("page content-type %q is not text/html", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchReferenceMaxBytes+1))
	if err != nil {
		return nil, nil, false, fmt.Errorf("read page body: %w", err)
	}
	truncated := len(body) > fetchReferenceMaxBytes
	if truncated {
		body = body[:fetchReferenceMaxBytes]
	}
	finalURL, perr := url.Parse(resp.Request.RawURL)
	if perr != nil {
		return nil, nil, false, fmt.Errorf("parse final url: %w", perr)
	}
	return body, finalURL, truncated, nil
}

// inlineOneStylesheet validates href against the SSRF guard, fetches the
// stylesheet, minifies it, and replaces sel with the resulting <style> tag.
// Errors are non-fatal: the original <link> is left in place.
func inlineOneStylesheet(ctx context.Context, client *req.Client, m *minify.M, base *url.URL, sel *goquery.Selection) bool {
	href, ok := sel.Attr("href")
	if !ok || strings.TrimSpace(href) == "" {
		return false
	}
	ref, err := url.Parse(href)
	if err != nil {
		return false
	}
	abs := base.ResolveReference(ref)
	_, err = validateReferenceURL(abs.String(), nil)
	if err != nil {
		slog.Debug("agent.fetch_reference.skip_stylesheet", "url", abs.String(), "err", err)
		return false
	}
	css, err := fetchStylesheet(ctx, client, abs.String())
	if err != nil {
		slog.Debug("agent.fetch_reference.skip_stylesheet", "url", abs.String(), "err", err)
		return false
	}
	var out bytes.Buffer
	err = mincss.Minify(m, &out, strings.NewReader(css), nil)
	if err != nil {
		slog.Debug("agent.fetch_reference.skip_stylesheet", "url", abs.String(), "err", err)
		return false
	}
	sel.ReplaceWithHtml("<style>" + out.String() + "</style>")
	return true
}

// fetchAndInline GETs target, parses it as HTML, and replaces each
// <link rel="stylesheet"> with an inline <style> containing the minified CSS.
func fetchAndInline(ctx context.Context, client *req.Client, target string) (string, string, bool, error) {
	body, finalURL, pageTruncated, err := fetchPage(ctx, client, target)
	if err != nil {
		return "", "", false, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return "", finalURL.String(), false, fmt.Errorf("parse html: %w", err)
	}
	m := minify.New()
	inlined := 0
	doc.Find("link[rel='stylesheet']").Each(func(_ int, sel *goquery.Selection) {
		if inlined >= fetchReferenceMaxStylesheets {
			return
		}
		if inlineOneStylesheet(ctx, client, m, finalURL, sel) {
			inlined++
		}
	})
	html, err := doc.Html()
	if err != nil {
		return "", finalURL.String(), false, fmt.Errorf("render html: %w", err)
	}
	truncated := pageTruncated
	if len(html) > fetchReferenceMaxBytes {
		html = html[:fetchReferenceMaxBytes]
		truncated = true
	}
	return html, finalURL.String(), truncated, nil
}

func fetchStylesheet(ctx context.Context, client *req.Client, target string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, fetchReferenceRequestTimeout)
	defer cancel()
	resp, err := client.R().SetContext(rctx).Get(target)
	if err != nil {
		return "", fmt.Errorf("get stylesheet: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "text/css") {
		return "", fmt.Errorf("content-type %q is not text/css", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchReferenceMaxStylesheetBytes+1))
	if err != nil {
		return "", fmt.Errorf("read stylesheet body: %w", err)
	}
	if len(body) > fetchReferenceMaxStylesheetBytes {
		return "", fmt.Errorf("stylesheet exceeds %d bytes", fetchReferenceMaxStylesheetBytes)
	}
	return string(body), nil
}

// newFetchReferenceTool registers fetch_reference, which lets the agent pull
// an external URL as design inspiration. Validation and rendering errors are
// returned in the Error field so the agent can recover within the run.
func newFetchReferenceTool(emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "fetch_reference"}
	client := newFetchReferenceClient()
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "fetch_reference",
			Description: "Fetch an external URL as design inspiration. GETs the page, inlines linked stylesheets (minified), and returns the resulting HTML (line-numbered, same convention as read_file/read_attachment). JavaScript is NOT executed — single-page-app shells come back mostly empty, so prefer server-rendered or static pages. Use sparingly (one or two URLs per session). Treat the result as a reference for layout, palette, and typography; never copy markup verbatim, and keep your output inline-only with no external CDNs other than the design substrate.",
		},
		func(tctx tool.Context, args fetchReferenceArgs) (fetchReferenceResult, error) {
			em.start(args.URL)
			parsed, err := validateReferenceURL(args.URL, nil)
			if err != nil {
				em.fail(args.URL, err)
				return fetchReferenceResult{Error: err.Error()}, nil
			}
			ctx, cancel := context.WithTimeout(tctx, fetchReferenceTimeout)
			defer cancel()
			html, finalURL, truncated, ferr := fetchAndInline(ctx, client, parsed.String())
			if ferr != nil {
				slog.Warn("agent.fetch_reference", "url", parsed.String(), "err", ferr)
				em.fail(args.URL, ferr)
				return fetchReferenceResult{FinalURL: finalURL, Error: ferr.Error()}, nil
			}
			slog.Info("agent.fetch_reference",
				"url", parsed.String(),
				"final_url", finalURL,
				"length", len(html),
				"truncated", truncated)
			em.done(args.URL)
			return fetchReferenceResult{
				Content:   NumberLines(html, 1),
				FinalURL:  finalURL,
				Truncated: truncated,
			}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create fetch_reference tool: %w", err)
	}
	return t, nil
}
