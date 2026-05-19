package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/alecthomas/kong"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/crypto/bcrypt"

	"github.com/jtarchie/buildabear/internal/build"
	"github.com/jtarchie/buildabear/internal/events"
	"github.com/jtarchie/buildabear/internal/model"
	"github.com/jtarchie/buildabear/internal/sandbox"
	"github.com/jtarchie/buildabear/internal/server"
	"github.com/jtarchie/buildabear/internal/snapshot"
	"github.com/jtarchie/buildabear/internal/state"
	"github.com/jtarchie/buildabear/internal/store"
)

var cli struct {
	Port   string `default:"8080"      env:"PORT"   help:"HTTP port to listen on."`
	Domain string `default:"localhost" env:"DOMAIN" help:"Base domain for subdomains."`

	AdminUsername string `default:"admin"      env:"ADMIN_USERNAME"                                           help:"Username for the admin HTTP Basic Auth gate." name:"admin-username"`
	AdminPassword string `env:"ADMIN_PASSWORD" help:"Password for the admin HTTP Basic Auth gate (required)." name:"admin-password"                               required:""`

	S3Bucket      string `env:"S3_BUCKET"        help:"S3 bucket name (multi-tenant)."     name:"s3-bucket"       required:""`
	S3EndpointURL string `env:"AWS_ENDPOINT_URL" help:"Override S3 endpoint (e.g. Minio)." name:"s3-endpoint-url"`

	CacheSize int `default:"1024" env:"CACHE_SIZE" help:"Number of items to cache in ARC." name:"cache-size"`

	SnapshotKeep int `default:"100" env:"SNAPSHOT_KEEP" help:"Max snapshot archives to retain per site (0 disables retention)." name:"snapshot-keep"`

	EditsKeep   int  `default:"50"   env:"EDITS_KEEP"   help:"Max per-edit transcripts to retain per site (0 disables retention; transcripts are still written)." name:"edits-keep"`
	RecordEdits bool `default:"true" env:"RECORD_EDITS" help:"Capture per-edit transcripts to _edits/{slug}/."                                                    name:"record-edits"`

	BuildTimeout time.Duration `default:"15m" env:"BUILD_TIMEOUT" help:"Wall-clock cap per build (initial agent run plus any lint retries). Bump for slower local models; lower for cloud-only deployments." name:"build-timeout"`

	LLMModel        string `default:"lmstudio/google/gemma-4-26b-a4b" env:"LLM_MODEL"                                help:"LLM model as provider/model-name."                                                                                                                  name:"llm-model"`
	LLMAPIKey       string `env:"LLM_API_KEY"                         help:"API key for the LLM provider."           name:"llm-api-key"`
	LLMBaseURL      string `env:"LLM_BASE_URL"                        help:"Override base URL for the LLM provider." name:"llm-base-url"`
	ReasoningEffort string `default:""                                env:"REASONING_EFFORT"                         help:"Ask the model to reason before responding. One of: none|minimal|low|medium|high. Empty / 'none' disables. Only useful on reasoning-capable models." name:"reasoning-effort"`

	// ACME / Let's Encrypt. ACMEEmail is the toggle: empty = plain HTTP on
	// --port (dev), non-empty = bind 80+443 and serve TLS via autocert with
	// certs persisted in S3 under ACMECachePrefix.
	ACMEEmail       string `env:"ACME_EMAIL"     help:"Contact email for Let's Encrypt. When set, enables TLS via autocert on --http-port and --tls-port."                                                                                             name:"acme-email"`
	ACMECachePrefix string `default:"_acme/"     env:"ACME_CACHE_PREFIX"                                                                                                                                                                               help:"S3 key prefix for ACME account key + cert cache."                                               name:"acme-cache-prefix"`
	ACMEDirectory   string `env:"ACME_DIRECTORY" help:"Override the ACME directory URL (e.g. LE staging during cutover). Empty uses Let's Encrypt production."                                                                                         name:"acme-directory"`
	TLSPort         string `default:"443"        env:"TLS_PORT"                                                                                                                                                                                        help:"Port for the HTTPS listener (when --acme-email is set)."                                        name:"tls-port"`
	HTTPPort        string `default:"80"         env:"HTTP_PORT"                                                                                                                                                                                       help:"Port for the HTTP listener that serves ACME challenges + redirects (when --acme-email is set)." name:"http-port"`
	ProxyProtocol   bool   `env:"PROXY_PROTOCOL" help:"Parse PROXY protocol v1/v2 headers on incoming connections. Required on Fly Machines services declared with handlers=[\"proxy_proto\"] (the only way to get raw-TCP pass-through on port 443)." name:"proxy-protocol"`
}

func main() {
	kong.Parse(&cli,
		kong.Name("buildabear"),
		kong.Description("Vibe coding app hosting platform."),
		kong.UsageOnError(),
	)

	ctx := context.Background()

	provider, name := model.SplitModel(cli.LLMModel)
	llm, err := model.Resolve(provider, name, cli.LLMAPIKey, cli.LLMBaseURL)
	if err != nil {
		slog.Error("model resolve failed", "err", err)
		os.Exit(1)
	}

	reasoningEffort, err := model.ParseReasoningEffort(cli.ReasoningEffort)
	if err != nil {
		slog.Error("reasoning effort invalid", "err", err)
		os.Exit(1)
	}

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		slog.Error("aws config failed", "err", err)
		os.Exit(1)
	}

	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if cli.S3EndpointURL != "" {
			o.BaseEndpoint = aws.String(cli.S3EndpointURL)
			o.UsePathStyle = true
		}
	})

	s, err := store.New(s3Client, cli.S3Bucket, cli.CacheSize)
	if err != nil {
		slog.Error("store initialization failed", "err", err)
		os.Exit(1)
	}

	err = s.EnsureBucket(ctx)
	if err != nil {
		slog.Error("ensure bucket failed", "bucket", cli.S3Bucket, "err", err)
		os.Exit(1)
	}

	tracker := events.NewTracker()
	snapshotSvc := snapshot.New(s, cli.SnapshotKeep)
	buildSvc := build.NewWithConfig(build.Config{
		Store:           s,
		LLM:             llm,
		Model:           cli.LLMModel,
		Events:          tracker,
		Snapshot:        snapshotSvc,
		EditsKeep:       cli.EditsKeep,
		RecordEdit:      cli.RecordEdits,
		BuildTimeout:    cli.BuildTimeout,
		ReasoningEffort: reasoningEffort,
	})
	sb := sandbox.New(sandbox.Config{})
	stateStore := state.NewS3(s3Client, cli.S3Bucket)

	adminHash, err := bcrypt.GenerateFromPassword([]byte(cli.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		slog.Error("admin password hash failed", "err", err)
		os.Exit(1)
	}

	deps := server.Deps{
		Store:             s,
		Build:             buildSvc,
		Events:            tracker,
		LLM:               llm,
		Sandbox:           sb,
		State:             stateStore,
		Snapshot:          snapshotSvc,
		Domain:            cli.Domain,
		Port:              cli.Port,
		AdminUsername:     cli.AdminUsername,
		AdminPasswordHash: string(adminHash),
		SystemInfo: server.SystemInfo{
			LLMModel:           cli.LLMModel,
			LLMBaseURL:         cli.LLMBaseURL,
			LLMReasoningEffort: cli.ReasoningEffort,
			S3Endpoint:         cli.S3EndpointURL,
			S3Bucket:           cli.S3Bucket,
			SnapshotKeep:       cli.SnapshotKeep,
			EditsKeep:          cli.EditsKeep,
		},
	}

	// Plain-HTTP path (dev / no ACME). echo.Start blocks until shutdown.
	if cli.ACMEEmail == "" {
		e, _ := server.New(deps)
		slog.Info("app.started", "port", cli.Port, "domain", cli.Domain, "model", cli.LLMModel, "tls", false)
		err = e.Start(":" + cli.Port)
		if err != nil {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
		return
	}

	// TLS path. Order matters: build the autocert manager first so we can
	// wire its pre-warm callback into Deps; the manager's HostPolicy needs
	// the *Server, so patch it after construction.
	tlsOpts := server.TLSOpts{
		Cache:         store.NewACMECache(s, cli.ACMECachePrefix),
		Email:         cli.ACMEEmail,
		HTTPPort:      cli.HTTPPort,
		TLSPort:       cli.TLSPort,
		Directory:     cli.ACMEDirectory,
		ProxyProtocol: cli.ProxyProtocol,
	}
	mgr := server.NewAutocertManager(tlsOpts)
	deps.PreWarmCert = func(host string) { server.PreWarm(mgr, host) }

	e, srv := server.New(deps)
	mgr.HostPolicy = func(_ context.Context, host string) error {
		if srv.HostAllowed(host) {
			return nil
		}
		return fmt.Errorf("acme: host %q not configured", host)
	}

	slog.Info("app.started", "tls_port", cli.TLSPort, "http_port", cli.HTTPPort,
		"domain", cli.Domain, "model", cli.LLMModel, "tls", true,
		"acme_directory", cli.ACMEDirectory)
	err = server.RunWithTLS(ctx, e, mgr, tlsOpts)
	if err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
