package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/alecthomas/kong"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/buildabear/internal/model"
)

var cli struct {
	Port   string `help:"HTTP port to listen on."           default:"8080"                              env:"PORT"`
	Domain string `help:"Base domain for subdomains."       default:"localhost"                         env:"DOMAIN"`

	S3Bucket      string `help:"S3 bucket name (multi-tenant)."    required:""                                 env:"S3_BUCKET"       name:"s3-bucket"`
	S3EndpointURL string `help:"Override S3 endpoint (e.g. Minio)."                                            env:"AWS_ENDPOINT_URL" name:"s3-endpoint-url"`

	LLMModel  string `help:"LLM model as provider/model-name." default:"lmstudio/google/gemma-4-26b-a4b" env:"LLM_MODEL"       name:"llm-model"`
	LLMAPIKey string `help:"API key for the LLM provider."                                                 env:"LLM_API_KEY"     name:"llm-api-key"`
	LLMBaseURL string `help:"Override base URL for the LLM provider."                                      env:"LLM_BASE_URL"    name:"llm-base-url"`
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

	store := NewStore(s3Client, cli.S3Bucket)
	if err := store.EnsureBucket(ctx); err != nil {
		slog.Error("ensure bucket failed", "bucket", cli.S3Bucket, "err", err)
		os.Exit(1)
	}

	e := NewServer(store, cli.Domain, cli.Port, llm)
	e.Server.WriteTimeout = 10 * time.Minute
	e.Server.ReadTimeout = 30 * time.Second
	e.Server.IdleTimeout = 60 * time.Second

	slog.Info("app.started", "port", cli.Port, "domain", cli.Domain, "model", cli.LLMModel)
	if err := e.Start(":" + cli.Port); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
