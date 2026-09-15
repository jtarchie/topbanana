// Package linkcheck probes a site's external links from the server; advisory only, never the lint gate, because third-party uptime is not deterministic.
package linkcheck

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/html"

	"github.com/jtarchie/topbanana/internal/lint"
	"github.com/jtarchie/topbanana/internal/netguard"
	"github.com/jtarchie/topbanana/internal/store"
)

type Status string

const (
	StatusDead       Status = "dead"       // certain: no such domain, or 404/410
	StatusSuspect    Status = "suspect"    // may be transient: 5xx, timeout, refused, bad certificate
	StatusUnverified Status = "unverified" // can't judge from here: a bot wall (401/403/429/999) or a private address
	StatusUnchecked  Status = "unchecked"
	StatusOK         Status = "ok"
)

var severity = map[Status]int{StatusDead: 0, StatusSuspect: 1, StatusUnverified: 2, StatusUnchecked: 3, StatusOK: 4}

// A dead answer expires sooner than an ok one so a repaired link clears within a day.
var ttl = map[Status]time.Duration{
	StatusOK:         7 * 24 * time.Hour,
	StatusUnverified: 7 * 24 * time.Hour,
	StatusDead:       24 * time.Hour,
	StatusSuspect:    6 * time.Hour,
}

const (
	maxLinks       = 200 // ponytail: links past this go unchecked; paginate if a site ever needs more
	checkBudget    = 30 * time.Second
	requestTimeout = 8 * time.Second
	concurrency    = 8
	userAgent      = "Mozilla/5.0 (compatible; TopBananaLinkCheck/1.0)"
	canaryHost     = "example.com"
)

type Result struct {
	URL       string    `json:"url"`
	Status    Status    `json:"status"`
	Code      int       `json:"code,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Pages     []string  `json:"-"`
}

type Report struct {
	Results []Result // worst first
	Stale   bool     // some answer is missing or expired
}

func (r Report) Count(s Status) int {
	n := 0
	for _, res := range r.Results {
		if res.Status == s {
			n++
		}
	}
	return n
}

func (r Report) Dead() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Status == StatusDead {
			out = append(out, res)
		}
	}
	return out
}

type Checker struct {
	store  *store.Store
	client *http.Client
	now    func() time.Time
	canary func(ctx context.Context) error

	mu      sync.Mutex
	running map[string]bool
}

func New(s *store.Store) *Checker { return newChecker(s, netguard.Control) }

func newChecker(s *store.Store, control func(network, address string, c syscall.RawConn) error) *Checker {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: control}
	transport := &http.Transport{
		Proxy:                  nil, // an env proxy would move the dial, and so the guard, off this process
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  requestTimeout,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConnsPerHost:    2,
	}
	return &Checker{
		store: s,
		client: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
		now:     time.Now,
		canary:  resolveCanary,
		running: map[string]bool{},
	}
}

func resolveCanary(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := net.DefaultResolver.LookupHost(ctx, canaryHost)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", canaryHost, err)
	}
	return nil
}

// linkAttrs is navigations and embedded media only: preconnect hints and form actions answer 404/405 to a bare GET while working fine.
var linkAttrs = map[string]map[string]bool{
	"a": {"href": true}, "area": {"href": true},
	"img": {"src": true}, "iframe": {"src": true}, "embed": {"src": true},
	"video": {"src": true, "poster": true}, "audio": {"src": true}, "source": {"src": true}, "track": {"src": true},
}

// Links maps each external URL on the site to the pages that use it.
func Links(ctx context.Context, s *store.Store, slug string) (map[string][]string, error) {
	files, err := s.List(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", slug, err)
	}
	out := map[string][]string{}
	for _, f := range files {
		if !strings.HasSuffix(f, ".html") {
			continue
		}
		obj, err := s.Read(ctx, slug, f)
		if err != nil {
			continue
		}
		doc, err := html.Parse(strings.NewReader(obj.Content))
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		lint.WalkDOM(doc, func(n *html.Node) {
			if n.Type != html.ElementNode {
				return
			}
			for _, a := range n.Attr {
				if !linkAttrs[n.Data][a.Key] {
					continue
				}
				u, ok := normalize(a.Val)
				if !ok || seen[u] {
					continue
				}
				seen[u] = true
				if _, known := out[u]; !known && len(out) >= maxLinks {
					continue
				}
				out[u] = append(out[u], f)
			}
		})
	}
	return out, nil
}

func normalize(raw string) (string, bool) {
	v := strings.TrimSpace(raw)
	if strings.HasPrefix(v, "//") {
		v = "https:" + v
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	u.Fragment = ""
	return u.String(), true
}

// Cached answers from the cache alone, so rendering a page never waits on a third-party host.
func (c *Checker) Cached(ctx context.Context, slug string) (Report, error) {
	links, err := Links(ctx, c.store, slug)
	if err != nil {
		return Report{}, err
	}
	return c.report(ctx, links, false), nil
}

func (c *Checker) Check(ctx context.Context, slug string) (Report, error) {
	links, err := Links(ctx, c.store, slug)
	if err != nil {
		return Report{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()
	return c.report(ctx, links, true), nil
}

// RefreshAsync re-checks in the background, once per slug at a time; a page view triggers it so rot surfaces without a scheduler.
func (c *Checker) RefreshAsync(slug string) {
	c.mu.Lock()
	if c.running[slug] {
		c.mu.Unlock()
		return
	}
	c.running[slug] = true
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.running, slug)
			c.mu.Unlock()
		}()
		_, err := c.Check(context.Background(), slug)
		if err != nil {
			slog.Warn("linkcheck.refresh_failed", "slug", slug, "err", err)
		}
	}()
}

func (c *Checker) report(ctx context.Context, links map[string][]string, probe bool) Report {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		rep     Report
		sem     = make(chan struct{}, concurrency)
		hostMus = map[string]*sync.Mutex{} // one request per host at a time, so 40 links to one domain aren't a flood
		// An offline resolver answers "no such host" for every name, so that answer only counts once a known-good name resolves.
		dnsUp = sync.OnceValue(func() bool { return c.canary(context.WithoutCancel(ctx)) == nil })
	)
	for u, pages := range links {
		host := hostOf(u)
		if hostMus[host] == nil {
			hostMus[host] = &sync.Mutex{}
		}
		hostMu := hostMus[host]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			// A lookup queued behind 30s of probes must not inherit the spent budget, or a fresh cached answer reads as unchecked.
			r, found := c.lookup(context.WithoutCancel(ctx), u)
			<-sem
			stale := !found || c.now().Sub(r.CheckedAt) > ttl[r.Status]
			if !found {
				r = Result{URL: u, Status: StatusUnchecked}
			}
			if probe && stale {
				hostMu.Lock()
				sem <- struct{}{}
				fresh, err := c.probe(ctx, u)
				<-sem
				hostMu.Unlock()
				// A probe the budget cut short, or an NXDOMAIN while DNS itself is down, says nothing about the link; keep the old answer.
				if ctx.Err() == nil && (!nxdomain(err) || dnsUp()) {
					r, stale = fresh, false
					c.save(ctx, r)
				}
			}
			r.Pages = pages
			mu.Lock()
			rep.Results = append(rep.Results, r)
			rep.Stale = rep.Stale || stale
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(rep.Results, func(i, j int) bool {
		a, b := rep.Results[i], rep.Results[j]
		if severity[a.Status] != severity[b.Status] {
			return severity[a.Status] < severity[b.Status]
		}
		return a.URL < b.URL
	})
	return rep
}

func hostOf(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	return parsed.Hostname()
}

func cacheKey(u string) string {
	sum := sha256.Sum256([]byte(u))
	return store.LinkCheckPrefix + hex.EncodeToString(sum[:]) + ".json"
}

func (c *Checker) lookup(ctx context.Context, u string) (Result, bool) {
	obj, err := c.store.ReadRaw(ctx, cacheKey(u))
	if err != nil || obj.Content == "" {
		return Result{}, false
	}
	var r Result
	if json.Unmarshal([]byte(obj.Content), &r) != nil || r.URL != u {
		return Result{}, false
	}
	return r, true
}

func (c *Checker) save(ctx context.Context, r Result) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	err = c.store.WriteRaw(context.WithoutCancel(ctx), cacheKey(r.URL), string(b), "application/json", nil)
	if err != nil {
		slog.Warn("linkcheck.cache_write_failed", "url", r.URL, "err", err)
	}
}

func (c *Checker) probe(ctx context.Context, u string) (Result, error) {
	code, err := c.fetch(ctx, http.MethodHead, u)
	// Many servers reject or mis-answer HEAD, so only a clean HEAD or a failure a GET can't change is final.
	if (err == nil && code >= 400) || (err != nil && !certain(err)) {
		code, err = c.fetch(ctx, http.MethodGet, u)
	}
	status, reason := classify(code, err)
	return Result{URL: u, Status: status, Code: code, Reason: reason, CheckedAt: c.now()}, err
}

func (c *Checker) fetch(ctx context.Context, method, u string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err //nolint:wrapcheck // classify inspects the chain
	}
	_ = resp.Body.Close() // the status is the answer; never read a body the remote sizes
	return resp.StatusCode, nil
}

func certain(err error) bool {
	return errors.Is(err, netguard.ErrBlocked) || nxdomain(err)
}

func nxdomain(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

func classify(code int, err error) (Status, string) {
	if err != nil {
		var certErr *tls.CertificateVerificationError
		var netErr net.Error
		switch {
		case errors.Is(err, netguard.ErrBlocked):
			// A router guide's 192.168.0.1 or a tutorial's localhost works for the reader; the agent must never be told to delete it.
			return StatusUnverified, "a private or local address, reachable only on the visitor's own network"
		case nxdomain(err):
			return StatusDead, "the domain does not exist"
		case errors.As(err, &certErr):
			return StatusSuspect, "the site's security certificate is invalid"
		case errors.Is(err, syscall.ECONNREFUSED):
			return StatusSuspect, "the server refused the connection"
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
			return StatusSuspect, "the site did not answer in time"
		default:
			return StatusSuspect, "could not connect"
		}
	}
	switch {
	case code < 400:
		return StatusOK, ""
	case code == http.StatusNotFound, code == http.StatusGone:
		return StatusDead, fmt.Sprintf("page not found (%d)", code)
	case code == http.StatusUnauthorized, code == http.StatusForbidden, code == http.StatusTooManyRequests, code == 999:
		return StatusUnverified, fmt.Sprintf("the site blocks automated checks (%d)", code)
	case code >= 500:
		return StatusSuspect, fmt.Sprintf("the site had a server error (%d)", code)
	default:
		return StatusSuspect, fmt.Sprintf("unexpected response (%d)", code)
	}
}
