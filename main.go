package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/alecthomas/kong"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/buildabear/internal/model"
)

var cli struct {
	Port   string `default:"8080"      env:"PORT"   help:"HTTP port to listen on."`
	Domain string `default:"localhost" env:"DOMAIN" help:"Base domain for subdomains."`

	S3Bucket      string `env:"S3_BUCKET"        help:"S3 bucket name (multi-tenant)."     name:"s3-bucket"       required:""`
	S3EndpointURL string `env:"AWS_ENDPOINT_URL" help:"Override S3 endpoint (e.g. Minio)." name:"s3-endpoint-url"`

	CacheSize int `default:"100" env:"CACHE_SIZE" help:"Number of items to cache in ARC." name:"cache-size"`

	LLMModel   string `default:"lmstudio/google/gemma-4-26b-a4b" env:"LLM_MODEL"                                help:"LLM model as provider/model-name." name:"llm-model"`
	LLMAPIKey  string `env:"LLM_API_KEY"                         help:"API key for the LLM provider."           name:"llm-api-key"`
	LLMBaseURL string `env:"LLM_BASE_URL"                        help:"Override base URL for the LLM provider." name:"llm-base-url"`
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

	store, err := NewStore(s3Client, cli.S3Bucket, cli.CacheSize)
	if err != nil {
		slog.Error("store initialization failed", "err", err)
		os.Exit(1)
	}

	err = store.EnsureBucket(ctx)
	if err != nil {
		slog.Error("ensure bucket failed", "bucket", cli.S3Bucket, "err", err)
		os.Exit(1)
	}

	e := NewServer(store, cli.Domain, cli.Port, llm)

	slog.Info("app.started", "port", cli.Port, "domain", cli.Domain, "model", cli.LLMModel)
	err = e.Start(":" + cli.Port)
	if err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
