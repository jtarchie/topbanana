package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jtarchie/mailcatch/smtpd"

	"github.com/jtarchie/topbanana/internal/mail"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/store"
)

// newMailStore returns nil when SMTP is off, which hides the whole Mail surface.
func newMailStore(s *store.Store) *mail.Store {
	if cli.SMTPAddr == "" {
		return nil
	}
	return mail.New(s, mail.Options{})
}

func smtpHostname() string {
	if cli.SMTPHostname != "" {
		return cli.SMTPHostname
	}
	return "mx." + cli.Domain
}

// startSMTP binds before returning so a bad --smtp-addr fails startup, then serves in the background; a nil store means mail is off.
// ponytail: no graceful drain on shutdown; senders retry a dropped session, so add Shutdown wiring only if deploys show 4xx bursts.
func startSMTP(ctx context.Context, ms *mail.Store, srv *server.Server, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) error {
	if ms == nil {
		return nil
	}
	host := smtpHostname()
	tlsConfig, err := smtpTLS(host, getCert)
	if err != nil {
		return err
	}
	cfg := smtpd.Config{
		Hostname: host,
		Resolve: func(domain string) (smtpd.Route, bool) {
			slug, ttl, ok := srv.MailRoute(domain)
			return smtpd.Route{Tenant: slug, TTL: ttl}, ok
		},
		TLSConfig:     tlsConfig,
		ProxyProtocol: cli.SMTPProxyProtocol,
		Logger:        slog.Default(),
	}
	newSMTP := smtpd.NewGoSMTP
	if cli.SMTPImpl == "ragel" {
		newSMTP = smtpd.NewRagel
	}
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cli.SMTPAddr)
	if err != nil {
		return fmt.Errorf("smtp listen %s: %w", cli.SMTPAddr, err)
	}
	go ms.RunReaper(ctx, time.Minute)
	go func() {
		serr := newSMTP(cfg, ms).Serve(l)
		if serr != nil {
			slog.Error("smtp.serve", "err", serr)
		}
	}()
	slog.Info("smtp.started", "addr", l.Addr().String(), "hostname", host, "impl", cli.SMTPImpl)
	return nil
}

// smtpTLS prefers the autocert certificate for the MX host and falls back to self-signed: senders encrypt without verifying MX certs, so a failed issuance must never cost STARTTLS.
func smtpTLS(host string, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (*tls.Config, error) {
	self, err := smtpd.SelfSignedTLS(host)
	if err != nil {
		return nil, fmt.Errorf("smtp self-signed cert: %w", err)
	}
	if getCert == nil {
		return self, nil
	}
	fallback := self.Certificates[0]
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// Senders often omit SNI; the MX host is the only name we'd answer to anyway.
			h := *hello
			h.ServerName = host
			cert, cerr := getCert(&h)
			if cerr != nil {
				slog.Warn("smtp.tls_fallback", "err", cerr)
				return &fallback, nil
			}
			return cert, nil
		},
	}, nil
}
