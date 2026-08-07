package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"
	proxyproto "github.com/pires/go-proxyproto"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/sync/errgroup"
)

// TLSOpts is what RunWithTLS needs to spin up the autocert-managed listener
// pair. Cache + HostPolicy come from the caller because the cache lives in
// internal/store and HostPolicy needs the live *Server.
type TLSOpts struct {
	Cache      autocert.Cache
	HostPolicy autocert.HostPolicy
	Email      string
	HTTPPort   string // for HTTP-01 challenges + HTTPS redirect, usually "80"
	TLSPort    string // for the TLS listener, usually "443"
	// Directory overrides the ACME endpoint (e.g. LE staging during cutover).
	// Empty uses autocert's default of LE production.
	Directory string
	// ProxyProtocol enables PROXY protocol v1/v2 header parsing on incoming
	// connections. Fly Machines requires `handlers = ["proxy_proto"]` to do
	// raw-TCP pass-through on port 443 (the API silently drops `handlers = []`
	// and defaults port 443 to TLS termination at the edge). The header
	// carries the visitor's real IP, which we restore to RemoteAddr so request
	// logs and rate limits see the right thing.
	ProxyProtocol bool

	// Tracker, when non-nil, wraps the TLS listener's GetCertificate so
	// on-demand issuance failures are recorded instead of vanishing into the
	// handshake. Pass the same instance handed to Deps.Certs.
	Tracker *CertTracker
}

// NewAutocertManager builds the autocert.Manager. Exported so callers can
// wrap GetCertificate for pre-warming custom domains right after a settings
// save — see the PreWarmCert callback in Deps.
func NewAutocertManager(opts TLSOpts) *autocert.Manager {
	m := &autocert.Manager{
		Cache:      opts.Cache,
		HostPolicy: opts.HostPolicy,
		Email:      opts.Email,
		Prompt:     autocert.AcceptTOS,
	}
	if opts.Directory != "" {
		m.Client = &acme.Client{DirectoryURL: opts.Directory}
	}
	return m
}

// RunWithTLS replaces echo.Echo.Start. Listens on two ports: HTTPPort serves
// ACME HTTP-01 challenges and 301s every other request to HTTPS; TLSPort
// terminates TLS using the autocert manager's per-host certs. Blocks until
// the context cancels (or SIGINT/SIGTERM), then drains both servers.
//
// We drop down to stdlib http.Server because Echo v5 removed StartAutoTLS /
// AutoTLSManager — the documented v5 idiom is to wire a vanilla http.Server
// with TLSConfig from autocert.Manager.TLSConfig().
func RunWithTLS(ctx context.Context, e *echo.Echo, m *autocert.Manager, opts TLSOpts) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tlsCfg := m.TLSConfig()
	// Force HTTP/2 + HTTP/1.1 on the TLS listener. autocert.Manager.TLSConfig
	// already includes acme.ALPNProto for TLS-ALPN-01 challenges; we add the
	// app protocols on top so browsers get h2 and ACME validators still work.
	tlsCfg.NextProtos = append([]string{"h2", "http/1.1"}, tlsCfg.NextProtos...)
	// Route certificate lookups through the tracker so a failed on-demand
	// issuance is retrievable afterwards. It delegates straight to the manager
	// — including the ALPN challenge path — and only records the outcome.
	if opts.Tracker != nil {
		tlsCfg.GetCertificate = opts.Tracker.GetCertificate
	}

	httpsSrv := &http.Server{
		Addr:              ":" + opts.TLSPort,
		Handler:           e,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpSrv := &http.Server{
		Addr:              ":" + opts.HTTPPort,
		Handler:           m.HTTPHandler(nil),
		ReadHeaderTimeout: 10 * time.Second,
	}

	httpsLn, err := listen(ctx, httpsSrv.Addr, opts.ProxyProtocol)
	if err != nil {
		return fmt.Errorf("https listen: %w", err)
	}
	httpLn, err := listen(ctx, httpSrv.Addr, opts.ProxyProtocol)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}

	group, gctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		slog.Info("tls.listen", "addr", httpsSrv.Addr, "proxy_protocol", opts.ProxyProtocol)
		err := httpsSrv.ServeTLS(httpsLn, "", "")
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("https serve: %w", err)
	})

	group.Go(func() error {
		slog.Info("http.listen", "addr", httpSrv.Addr, "proxy_protocol", opts.ProxyProtocol)
		err := httpSrv.Serve(httpLn)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http serve: %w", err)
	})

	group.Go(func() error {
		<-gctx.Done()
		// 25s leaves headroom under Fly's 30s SIGTERM-to-SIGKILL window.
		// Detach from gctx (which is already cancelled — that's why we're
		// here) so Shutdown actually waits for in-flight requests.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), 25*time.Second)
		defer cancel()
		err1 := httpsSrv.Shutdown(shutdownCtx)
		err2 := httpSrv.Shutdown(shutdownCtx)
		if err1 != nil {
			return fmt.Errorf("https shutdown: %w", err1)
		}
		if err2 != nil {
			return fmt.Errorf("http shutdown: %w", err2)
		}
		return nil
	})

	err = group.Wait()
	if err != nil {
		return fmt.Errorf("tls runner: %w", err)
	}
	return nil
}

// listen opens a TCP listener on addr. When proxyProtocol is true, the
// listener is wrapped so each incoming connection has its PROXY-protocol v1/v2
// header parsed and stripped; the original visitor IP is restored to
// RemoteAddr. Required for Fly Machines services declared with
// `handlers = ["proxy_proto"]`, which is the only way to get raw-TCP
// pass-through on port 443.
func listen(ctx context.Context, addr string, proxyProtocol bool) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	if !proxyProtocol {
		return ln, nil
	}
	return &proxyproto.Listener{
		Listener:          ln,
		ReadHeaderTimeout: 5 * time.Second,
	}, nil
}

// CertAttempt is the outcome of the most recent issuance attempt for a host.
// Err is the ACME error verbatim — "no such host", a CAA rejection, an
// unauthorized challenge — because the raw text is what identifies the
// misconfiguration, and paraphrasing it has no upside for the reader. An empty
// Err records a success, which is what clears a stale failure after a fix.
type CertAttempt struct {
	At  time.Time `json:"at"`
	Err string    `json:"err,omitempty"`
}

// CertProber is what the domain-status tools need from the TLS stack. Narrow
// on purpose: the server package holds a nil one in plain-HTTP/dev mode and
// reports "tls_disabled" rather than pretending to know about certificates.
type CertProber interface {
	// CachedCert returns the already-issued leaf certificate for host without
	// triggering issuance.
	CachedCert(ctx context.Context, host string) (*x509.Certificate, bool)
	// EnsureCert issues (or reuses) a certificate for host, returning the
	// underlying ACME error unwrapped when it fails.
	EnsureCert(ctx context.Context, host string) (*x509.Certificate, error)
	// LastAttempt reports the most recent issuance outcome for host, across
	// restarts and across instances.
	LastAttempt(ctx context.Context, host string) (CertAttempt, bool)
}

// Attempt records live beside the certificates in the ACME cache, under a
// prefix autocert never uses (its own keys are `{host}`, `{host}+rsa`,
// `{host}+token`, and `acme_account+key`). Sharing the cache means they
// inherit the same S3 backing and the same --acme-cache-prefix, so the
// diagnosis of a domain travels with the certificate it describes.
const certStatusKeyPrefix = "_status/"

// certStatusRefresh bounds how often an unchanged outcome is re-persisted.
// record() runs on every handshake for a host, so without coalescing a single
// broken domain under load turns one diagnostic into a write per TLS
// handshake. A changed error is always written immediately; an unchanged one
// only refreshes its timestamp this often.
const certStatusRefresh = 5 * time.Minute

// CertTracker wraps the autocert manager to make issuance observable.
// autocert issues lazily inside the TLS handshake and keeps no failure record,
// so a domain that will never get a certificate is indistinguishable from one
// nobody has visited yet — the single most expensive thing to diagnose about a
// custom domain, and the reason this type exists.
//
// Outcomes persist to the ACME cache, so a "failed" domain still reads as
// failed after a restart and reads the same from every instance — a certificate
// issued on one machine was always visible everywhere (it's in S3), and now the
// reason one *didn't* issue is too.
type CertTracker struct {
	mgr   *autocert.Manager
	cache autocert.Cache

	// attempts is the write-through cache over the persisted records: it
	// absorbs the per-handshake write rate and answers the common status call
	// without an S3 round-trip. Bounded by the set of hosts HostPolicy admits.
	mu       sync.Mutex
	attempts map[string]certAttemptState
}

// certAttemptState pairs an outcome with when it was last written down.
// persistedAt is tracked separately from CertAttempt.At because At advances on
// every handshake — comparing against it would mean an unchanged failure never
// refreshed its timestamp again.
type certAttemptState struct {
	attempt     CertAttempt
	persistedAt time.Time
}

var _ CertProber = (*CertTracker)(nil)

// NewCertTracker wraps a manager and the cache backing it. The cache is passed
// separately because autocert.Manager doesn't expose its own.
func NewCertTracker(m *autocert.Manager, cache autocert.Cache) *CertTracker {
	return &CertTracker{mgr: m, cache: cache, attempts: map[string]certAttemptState{}}
}

// GetCertificate is the TLS listener's hook. Delegates to the manager and
// records the outcome per SNI host.
func (t *CertTracker) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, err := t.mgr.GetCertificate(hello)
	if hello != nil && hello.ServerName != "" && !isACMEChallenge(hello) {
		t.record(helloContext(hello), hello.ServerName, err)
	}
	if err != nil {
		return nil, fmt.Errorf("get certificate: %w", err)
	}
	return cert, nil
}

// isACMEChallenge reports whether this handshake is Let's Encrypt validating a
// TLS-ALPN-01 challenge rather than a visitor fetching the site. Those succeed
// by returning the short-lived challenge certificate, so recording them would
// write a "success" in the middle of an issuance that may still fail — briefly
// clearing the very error someone is trying to read.
func isACMEChallenge(hello *tls.ClientHelloInfo) bool {
	return slices.Contains(hello.SupportedProtos, acme.ALPNProto)
}

// helloContext prefers the handshake's own context so a recorded outcome is
// cancelled with the connection. tls.ClientHelloInfo.Context() is nil unless a
// real handshake populated it (EnsureCert and tests synthesize the struct), so
// fall back rather than hand a nil context to the storage layer.
func helloContext(hello *tls.ClientHelloInfo) context.Context {
	if ctx := hello.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// EnsureCert forces an issuance attempt for host, the same way a first visitor
// would. Used by the settings-save pre-warm and by the check_domain tool.
//
// autocert runs issuance on its own background context, so ctx won't abort an
// in-flight ACME round-trip — it only bounds the recording of the outcome.
func (t *CertTracker) EnsureCert(ctx context.Context, host string) (*x509.Certificate, error) {
	cert, err := t.mgr.GetCertificate(&tls.ClientHelloInfo{ServerName: host})
	t.record(ctx, host, err)
	if err != nil {
		return nil, fmt.Errorf("issue certificate for %q: %w", host, err)
	}
	return leafOf(cert), nil
}

// CachedCert reads the issued certificate straight from the autocert cache, so
// a status call never triggers an ACME round-trip. Returns false when nothing
// has been issued for host yet.
func (t *CertTracker) CachedCert(ctx context.Context, host string) (*x509.Certificate, bool) {
	if t.cache == nil {
		return nil, false
	}
	// autocert keys the cache by bare hostname for its default ECDSA cert, and
	// normalizes the SNI the same way before doing so.
	data, err := t.cache.Get(ctx, normalizeCertHost(host))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	leaf := firstLeafFromPEM(data)
	if leaf == nil {
		return nil, false
	}
	return leaf, true
}

// LastAttempt returns the most recent recorded outcome for host, taking the
// newer of this instance's memory and the shared persisted record. Reading
// both is what makes the answer the same from every machine: another instance
// may have attempted more recently than this one, and a local record may be
// newer than what has been flushed.
func (t *CertTracker) LastAttempt(ctx context.Context, host string) (CertAttempt, bool) {
	host = normalizeCertHost(host)
	local, haveLocal := t.localAttempt(host)
	stored, haveStored := t.loadAttempt(ctx, host)
	switch {
	case haveLocal && haveStored:
		if stored.At.After(local.At) {
			return stored, true
		}
		return local, true
	case haveLocal:
		return local, true
	case haveStored:
		return stored, true
	}
	return CertAttempt{}, false
}

func (t *CertTracker) localAttempt(host string) (CertAttempt, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.attempts[host]
	return st.attempt, ok
}

// normalizeCertHost puts a host into the one form used for both the in-memory
// map key and the cache key. SNI arrives straight off the wire and hostnames
// are case-insensitive, so without this the same domain in two casings keeps
// two independent records — each with its own coalescing clock, both writing
// to the same (lowercased) object.
func normalizeCertHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// record notes the outcome of an issuance attempt, persisting it when the
// result is new, changed, or stale enough to be worth a refresh.
func (t *CertTracker) record(ctx context.Context, host string, err error) {
	// Only record hosts this deployment is configured to serve. SNI is
	// attacker-controlled and reaches here before any validation, so without
	// this gate an unauthenticated scanner could grow the map without bound
	// and drive one S3 write per novel hostname. HostPolicy is the same
	// in-memory check autocert itself applies, so this costs a map lookup.
	if !t.serves(ctx, host) {
		return
	}
	host = normalizeCertHost(host)
	attempt := CertAttempt{At: time.Now().UTC()}
	if err != nil {
		attempt.Err = err.Error()
		slog.Warn("acme.issue_failed", "host", host, "err", err)
	} else {
		slog.Info("acme.issue_ok", "host", host)
	}

	t.mu.Lock()
	prev, had := t.attempts[host]
	write := !had || prev.attempt.Err != attempt.Err ||
		time.Since(prev.persistedAt) >= certStatusRefresh
	// persistedAt carries over unchanged for now — it advances only once the
	// write actually lands, so a failed write leaves this attempt eligible to
	// be retried by the next one instead of being coalesced away.
	t.attempts[host] = certAttemptState{attempt: attempt, persistedAt: prev.persistedAt}
	t.mu.Unlock()

	if !write {
		return
	}
	perr := t.persistAttempt(ctx, host, attempt)
	if perr != nil {
		slog.Warn("acme.status_persist_failed", "host", host, "err", perr)
		return
	}
	t.markPersisted(host, attempt)
}

// markPersisted advances the coalescing clock after a successful write. It
// re-checks that the slot still holds the attempt we wrote: a newer attempt
// may have replaced it while the write was in flight, and that one has its own
// durability to earn.
func (t *CertTracker) markPersisted(host string, attempt CertAttempt) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.attempts[host]
	if !ok || !cur.attempt.At.Equal(attempt.At) {
		return
	}
	cur.persistedAt = attempt.At
	t.attempts[host] = cur
}

// serves reports whether the manager's HostPolicy admits host. A nil policy
// means autocert would issue for anything, so there's nothing to filter on.
func (t *CertTracker) serves(ctx context.Context, host string) bool {
	if t.mgr == nil || t.mgr.HostPolicy == nil {
		return true
	}
	return t.mgr.HostPolicy(ctx, host) == nil
}

// certStatusKey is the cache key holding host's last outcome. Hosts reaching
// here have passed HostPolicy, which only admits the configured domain, its
// slug subdomains, and registered custom domains — none of which can contain a
// path separator. The explicit check keeps that a property of this function
// rather than an inherited assumption, since the key concatenates into an S3
// object key.
func certStatusKey(host string) (string, bool) {
	switch {
	case host == "", len(host) > 253:
		return "", false
	case strings.ContainsAny(host, "/\\\x00"), strings.Contains(host, ".."):
		return "", false
	}
	return certStatusKeyPrefix + strings.ToLower(host), true
}

// certStatusWriteTimeout bounds the durability write. It runs on a context
// detached from the caller's: record() is reached from the TLS handshake, and
// a client that sends ClientHello then immediately resets the connection —
// exactly what a scanner does, and exactly when issuance fails — would
// otherwise cancel the write of the error we most want to keep.
const certStatusWriteTimeout = 5 * time.Second

func (t *CertTracker) persistAttempt(ctx context.Context, host string, attempt CertAttempt) error {
	if t.cache == nil {
		return nil
	}
	key, ok := certStatusKey(host)
	if !ok {
		return fmt.Errorf("refusing to build a status key for host %q", host)
	}
	body, err := json.Marshal(attempt)
	if err != nil {
		return fmt.Errorf("encode cert attempt: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), certStatusWriteTimeout)
	defer cancel()
	err = t.cache.Put(ctx, key, body)
	if err != nil {
		return fmt.Errorf("persist cert attempt: %w", err)
	}
	return nil
}

func (t *CertTracker) loadAttempt(ctx context.Context, host string) (CertAttempt, bool) {
	if t.cache == nil {
		return CertAttempt{}, false
	}
	key, ok := certStatusKey(host)
	if !ok {
		return CertAttempt{}, false
	}
	data, err := t.cache.Get(ctx, key)
	if err != nil || len(data) == 0 {
		return CertAttempt{}, false
	}
	var attempt CertAttempt
	err = json.Unmarshal(data, &attempt)
	if err != nil {
		slog.Warn("acme.status_decode_failed", "host", host, "err", err)
		return CertAttempt{}, false
	}
	return attempt, true
}

// leafOf returns the parsed leaf of a tls.Certificate, parsing it on demand
// when the handshake path didn't populate Leaf.
func leafOf(cert *tls.Certificate) *x509.Certificate {
	if cert == nil {
		return nil
	}
	if cert.Leaf != nil {
		return cert.Leaf
	}
	if len(cert.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil
	}
	return leaf
}

// firstLeafFromPEM pulls the leaf out of the PEM bundle autocert caches (the
// private key comes first, then the certificate chain leaf-first).
func firstLeafFromPEM(data []byte) *x509.Certificate {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil
		}
		return leaf
	}
}
