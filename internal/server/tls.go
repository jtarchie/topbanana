package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
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

// PreWarm asks the autocert manager to issue (or reuse) a cert for host. Safe
// to call from a goroutine after a settings save; errors are logged but not
// surfaced — the regular on-demand path will retry on the next visitor.
func PreWarm(m *autocert.Manager, host string) {
	_, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: host})
	if err != nil {
		slog.Warn("acme.prewarm_failed", "host", host, "err", err)
		return
	}
	slog.Info("acme.prewarm_ok", "host", host)
}
