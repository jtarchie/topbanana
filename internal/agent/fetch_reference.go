package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
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
	fetchReferenceTimeout  = 20 * time.Second
	fetchReferenceMaxBytes = maxHTMLFileBytes
	// fetchReferenceSettle gives client-side hydration a moment after the
	// body is ready. Short enough not to dominate the agent's iteration
	// budget; long enough that most SPAs paint their first frame.
	fetchReferenceSettle = 1500 * time.Millisecond
)

// inlineStylesheetsScript walks <link rel="stylesheet"> tags and replaces each
// with a <style> tag containing the rules from the matching CSSStyleSheet.
// Cross-origin sheets without CORS access throw on cssRules read; those links
// are left in place so the agent can still see what was referenced.
const inlineStylesheetsScript = `(() => {
  for (const link of [...document.querySelectorAll('link[rel="stylesheet"]')]) {
    try {
      const sheet = [...document.styleSheets].find(s => s.href === link.href);
      if (!sheet) continue;
      const rules = [...sheet.cssRules].map(r => r.cssText).join('\n');
      const style = document.createElement('style');
      style.textContent = rules;
      link.replaceWith(style);
    } catch (_) {}
  }
  return document.documentElement.outerHTML;
})()`

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
		if isBlockedIP(ip) {
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
		if isBlockedIP(ip) {
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

// renderReference navigates a fresh chromedp context to target, waits for the
// body and a brief settle window, then evaluates inlineStylesheetsScript and
// returns the resulting HTML plus the post-redirect URL.
//
// CHROMEDP_EXEC_PATH (optional) overrides the Chrome binary location — set
// in the container image where chromium lives at /usr/bin/chromium-browser.
// CHROMEDP_NO_SANDBOX=1 adds --no-sandbox and --disable-dev-shm-usage, which
// Chromium requires when running as root inside a container. Both are no-ops
// in local development, so macOS dev keeps the default Chrome detection.
func renderReference(ctx context.Context, target string) (string, string, error) {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	if path := os.Getenv("CHROMEDP_EXEC_PATH"); path != "" {
		opts = append(opts, chromedp.ExecPath(path))
	}
	if os.Getenv("CHROMEDP_NO_SANDBOX") == "1" {
		opts = append(opts,
			chromedp.NoSandbox,
			chromedp.Flag("disable-dev-shm-usage", true),
		)
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	var html, finalURL string
	err := chromedp.Run(browserCtx,
		chromedp.Navigate(target),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Sleep(fetchReferenceSettle),
		chromedp.Location(&finalURL),
		chromedp.Evaluate(inlineStylesheetsScript, &html),
	)
	if err != nil {
		return "", finalURL, fmt.Errorf("chromedp run: %w", err)
	}
	return html, finalURL, nil
}

// newFetchReferenceTool registers fetch_reference, which lets the agent pull
// an external URL as design inspiration. Validation and rendering errors are
// returned in the Error field so the agent can recover within the run.
func newFetchReferenceTool(emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "fetch_reference"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "fetch_reference",
			Description: "Fetch an external URL as design inspiration. Renders the page in a headless browser, inlines linked stylesheets, and returns the resulting HTML (line-numbered, same convention as read_file/read_attachment). Use sparingly — at most one or two URLs per session. Do NOT copy markup verbatim: your site must remain inline-only and dependency-free. Treat the result as a reference for layout, palette, and typography choices.",
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
			html, finalURL, err := renderReference(ctx, parsed.String())
			if err != nil {
				slog.Warn("agent.fetch_reference", "url", parsed.String(), "err", err)
				em.fail(args.URL, err)
				return fetchReferenceResult{FinalURL: finalURL, Error: err.Error()}, nil
			}
			truncated := false
			if len(html) > fetchReferenceMaxBytes {
				html = html[:fetchReferenceMaxBytes]
				truncated = true
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
