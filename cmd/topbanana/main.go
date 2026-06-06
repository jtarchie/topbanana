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
	adkmodel "google.golang.org/adk/model"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/model"
	"github.com/jtarchie/topbanana/internal/sandbox"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
	"github.com/jtarchie/topbanana/internal/store"
)

var cli struct {
	Port   string `default:"8080"      env:"PORT"   help:"HTTP port to listen on."`
	Domain string `default:"localhost" env:"DOMAIN" help:"Base domain for subdomains."`

	// CustomDomainCNAME is the hostname shown on the manage page as the target
	// for a customer's CNAME record. Empty falls back to --domain (the base
	// domain is where the server terminates TLS + routes custom domains).
	CustomDomainCNAME string `env:"CUSTOM_DOMAIN_CNAME" help:"CNAME target shown on the manage page for custom domains. Empty falls back to --domain." name:"custom-domain-cname"`

	// Multi-tenancy / passkeys. Required: every admin route is gated by a
	// passkey session bound to a user record, and the super admin is the
	// seeded role with platform-wide access.
	SuperAdminEmail string `env:"SUPER_ADMIN_EMAIL" help:"Email of the seeded super admin. Required — used to seed the first user and log the bootstrap invite URL on a fresh install."   name:"super-admin-email" required:""`
	InsecureCookies bool   `env:"INSECURE_COOKIES"  help:"Allow non-Secure cookies for local HTTP dev. Never set in production."                                                          name:"insecure-cookies"`

	// Per-user quotas. The defaults apply when a user record's Quotas
	// struct is zero-valued (no per-user override on /admin/users).
	DefaultMaxApps int `default:"0" env:"DEFAULT_MAX_APPS" help:"Default per-admin app cap when the user record has no specific limit. 0 = unlimited." name:"default-max-apps"`

	S3Bucket      string `env:"S3_BUCKET"        help:"S3 bucket name (multi-tenant)."     name:"s3-bucket"       required:""`
	S3EndpointURL string `env:"AWS_ENDPOINT_URL" help:"Override S3 endpoint (e.g. Minio)." name:"s3-endpoint-url"`

	CacheSize int `default:"1024" env:"CACHE_SIZE" help:"Number of items to cache in ARC." name:"cache-size"`

	SnapshotKeep int `default:"100" env:"SNAPSHOT_KEEP" help:"Max snapshot archives to retain per site (0 disables retention)." name:"snapshot-keep"`

	EditsKeep   int  `default:"50"   env:"EDITS_KEEP"   help:"Max per-edit transcripts to retain per site (0 disables retention; transcripts are still written)." name:"edits-keep"`
	RecordEdits bool `default:"true" env:"RECORD_EDITS" help:"Capture per-edit transcripts to _edits/{slug}/."                                                    name:"record-edits"`

	BuildTimeout time.Duration `default:"15m" env:"BUILD_TIMEOUT" help:"Wall-clock cap per build (initial agent run plus any lint retries). Bump for slower local models; lower for cloud-only deployments." name:"build-timeout"`

	TailwindCLI string `env:"TAILWIND_CLI" help:"Path to the Tailwind standalone binary used for the post-build per-site CSS compile. Empty falls back to a 'tailwindcss' or 'npx @tailwindcss/cli' on PATH; if none resolves, sites keep the CDN substrate tags." name:"tailwind-cli"`

	LLMModel        string `default:"lmstudio/google/gemma-4-26b-a4b" env:"LLM_MODEL"                                                                                                                                         help:"LLM model as provider/model-name. Used for the Author tier (initial site generation) and as the fallback for any tier-specific flag left unset."    name:"llm-model"`
	LLMEditorModel  string `env:"LLM_EDITOR_MODEL"                    help:"Model for the Editor tier: /edit, /relint, and lint-retry passes inside a build. Falls back to --llm-model when empty."                           name:"llm-editor-model"`
	LLMUtilityModel string `env:"LLM_UTILITY_MODEL"                   help:"Model for the Utility tier: post-build site-description summary. Falls back to --llm-model when empty."                                           name:"llm-utility-model"`
	LLMVisionModel  string `env:"LLM_VISION_MODEL"                    help:"Model for the Vision tier: image alt-text captioning. Falls back to --llm-model when empty — set explicitly when --llm-model isn't multimodal."   name:"llm-vision-model"`
	LLMAPIKey       string `env:"LLM_API_KEY"                         help:"API key for the LLM provider."                                                                                                                    name:"llm-api-key"`
	LLMBaseURL      string `env:"LLM_BASE_URL"                        help:"Override base URL for the LLM provider."                                                                                                          name:"llm-base-url"`
	ReasoningEffort string `default:""                                env:"REASONING_EFFORT"                                                                                                                                  help:"Ask the model to reason before responding. One of: none|minimal|low|medium|high. Empty / 'none' disables. Only useful on reasoning-capable models." name:"reasoning-effort"`

	// ACME / Let's Encrypt. ACMEEmail is the toggle: empty = plain HTTP on
	// --port (dev), non-empty = bind 80+443 and serve TLS via autocert with
	// certs persisted in S3 under ACMECachePrefix.
	ACMEEmail       string `env:"ACME_EMAIL"     help:"Contact email for Let's Encrypt. When set, enables TLS via autocert on --http-port and --tls-port."                                                                                             name:"acme-email"`
	ACMECachePrefix string `default:"_acme/"     env:"ACME_CACHE_PREFIX"                                                                                                                                                                               help:"S3 key prefix for ACME account key + cert cache."                                               name:"acme-cache-prefix"`
	ACMEDirectory   string `env:"ACME_DIRECTORY" help:"Override the ACME directory URL (e.g. LE staging during cutover). Empty uses Let's Encrypt production."                                                                                         name:"acme-directory"`
	TLSPort         string `default:"443"        env:"TLS_PORT"                                                                                                                                                                                        help:"Port for the HTTPS listener (when --acme-email is set)."                                        name:"tls-port"`
	HTTPPort        string `default:"80"         env:"HTTP_PORT"                                                                                                                                                                                       help:"Port for the HTTP listener that serves ACME challenges + redirects (when --acme-email is set)." name:"http-port"`
	ProxyProtocol   bool   `env:"PROXY_PROTOCOL" help:"Parse PROXY protocol v1/v2 headers on incoming connections. Required on Fly Machines services declared with handlers=[\"proxy_proto\"] (the only way to get raw-TCP pass-through on port 443)." name:"proxy-protocol"`

	// MCP server. When set, enables the Model Context Protocol endpoint at
	// /mcp plus its OAuth authorization server (so Claude Code can author
	// sites on behalf of a logged-in user). The value is the HMAC secret that
	// signs bearer tokens — keep it stable and private. Empty disables MCP.
	MCPSecret string `env:"MCP_SECRET" help:"HMAC secret that signs MCP bearer tokens. When set, enables the MCP server + OAuth endpoints at /mcp. Empty disables MCP." name:"mcp-secret"`
}

func main() {
	kong.Parse(&cli,
		kong.Name("topbanana"),
		kong.Description("Vibe coding app hosting platform."),
		kong.UsageOnError(),
	)

	ctx := context.Background()

	tierMap := model.TierMap{
		model.TierAuthor:  cli.LLMModel,
		model.TierEditor:  cli.LLMEditorModel,
		model.TierUtility: cli.LLMUtilityModel,
		model.TierVision:  cli.LLMVisionModel,
	}
	err := tierMap.Validate()
	if err != nil {
		slog.Error("model tier config invalid", "err", err)
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
	// llmFactory wires per-tier and per-user model dispatch: build.Service
	// invokes it on first use of a model ID, then caches the result. Two
	// tiers pointing at the same model share one client. Multi-key per-
	// provider support is a deliberate follow-up.
	llmFactory := func(_ context.Context, modelID string) (adkmodel.LLM, error) {
		provider, name := model.SplitModel(modelID)
		llm, err := model.Resolve(provider, name, cli.LLMAPIKey, cli.LLMBaseURL)
		if err != nil {
			return nil, fmt.Errorf("resolve model %q: %w", modelID, err)
		}
		return llm, nil
	}
	buildSvc := build.NewWithConfig(build.Config{
		Store:           s,
		TierMap:         tierMap,
		LLMFactory:      llmFactory,
		Events:          tracker,
		Snapshot:        snapshotSvc,
		EditsKeep:       cli.EditsKeep,
		RecordEdit:      cli.RecordEdits,
		BuildTimeout:    cli.BuildTimeout,
		ReasoningEffort: reasoningEffort,
		Domain:          cli.Domain,
		Port:            cli.Port,
		Insecure:        cli.InsecureCookies,
		TailwindCLI:     cli.TailwindCLI,
	})
	sb := sandbox.New(sandbox.Config{})
	stateStore := state.NewS3(s3Client, cli.S3Bucket)

	authSvc, err := auth.New(auth.Config{
		Store:           s,
		Domain:          cli.Domain,
		SuperAdminEmail: cli.SuperAdminEmail,
		InsecureCookies: cli.InsecureCookies,
		Port:            cli.Port,
		QuotaDefaults: auth.QuotaDefaults{
			MaxApps: cli.DefaultMaxApps,
			Tiers:   tierMap,
		},
	})
	if err != nil {
		slog.Error("auth init failed", "err", err)
		os.Exit(1)
	}
	_, err = authSvc.Bootstrap(ctx)
	if err != nil {
		slog.Error("auth bootstrap failed", "err", err)
		os.Exit(1)
	}
	// One-shot ownership migration: pre-multi-tenancy apps land with the
	// super admin as their owner so commit 5's per-slug authorization sees
	// every existing slug as accessible to them. Safe on every boot.
	err = authSvc.MigrateOwnership(ctx, &storeAppLister{s: s}, &buildMetaAdapter{b: buildSvc})
	if err != nil {
		slog.Error("auth migrate ownership failed", "err", err)
		os.Exit(1)
	}

	deps := server.Deps{
		Store:     s,
		Build:     buildSvc,
		Events:    tracker,
		Sandbox:   sb,
		State:     stateStore,
		Snapshot:  snapshotSvc,
		Auth:      authSvc,
		Domain:    cli.Domain,
		Port:      cli.Port,
		MCPSecret: cli.MCPSecret,
		SystemInfo: server.SystemInfo{
			LLMTiers:           tierMap,
			LLMBaseURL:         cli.LLMBaseURL,
			LLMReasoningEffort: cli.ReasoningEffort,
			S3Endpoint:         cli.S3EndpointURL,
			S3Bucket:           cli.S3Bucket,
			SnapshotKeep:       cli.SnapshotKeep,
			EditsKeep:          cli.EditsKeep,
			CustomDomainCNAME:  cli.CustomDomainCNAME,
		},
	}

	// Plain-HTTP path (dev / no ACME). echo.Start blocks until shutdown.
	if cli.ACMEEmail == "" {
		e, _ := server.New(deps)
		slog.Info("app.started", "port", cli.Port, "domain", cli.Domain, "tiers", tierMap, "tls", false)
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
		"domain", cli.Domain, "tiers", tierMap, "tls", true,
		"acme_directory", cli.ACMEDirectory)
	err = server.RunWithTLS(ctx, e, mgr, tlsOpts)
	if err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

// storeAppLister adapts *store.Store to auth.AppLister so the migration
// can enumerate slugs without internal/auth importing internal/store-as-a-concrete-type.
type storeAppLister struct{ s *store.Store }

func (a *storeAppLister) ListApps(ctx context.Context) ([]string, error) {
	apps, err := a.s.ListApps(ctx)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	return apps, nil
}

// buildMetaAdapter adapts *build.Service to auth.MetaAdapter so the
// migration can read and rewrite the per-app OwnerID without internal/auth
// pulling in internal/build.
type buildMetaAdapter struct{ b *build.Service }

func (a *buildMetaAdapter) ReadOwnerID(ctx context.Context, slug string) (string, bool) {
	meta := a.b.ReadMeta(ctx, slug)
	return meta.OwnerID, meta.Template != ""
}

func (a *buildMetaAdapter) SetOwnerID(ctx context.Context, slug, ownerID string) error {
	meta := a.b.ReadMeta(ctx, slug)
	meta.OwnerID = ownerID
	err := a.b.WriteMeta(ctx, slug, meta)
	if err != nil {
		return fmt.Errorf("write meta %s: %w", slug, err)
	}
	return nil
}
