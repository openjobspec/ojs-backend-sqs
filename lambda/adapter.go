// Package lambda constructs the HTTP surface of the OJS SQS backend for
// serverless runtimes. Adapter implements http.Handler, so callers can bridge
// it to their preferred API Gateway or Lambda HTTP adapter without importing
// repository-internal packages.
package lambda

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"github.com/openjobspec/ojs-backend-sqs/internal/metrics"
	"github.com/openjobspec/ojs-backend-sqs/internal/scheduler"
	"github.com/openjobspec/ojs-backend-sqs/internal/server"
	sqsbackend "github.com/openjobspec/ojs-backend-sqs/internal/sqs"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

// Option customizes Adapter construction.
type Option func(*options)

type options struct {
	logger    *slog.Logger
	awsConfig *aws.Config
}

// WithLogger sets the logger used by the backend and scheduler.
func WithLogger(logger *slog.Logger) Option {
	return func(opts *options) {
		opts.logger = logger
	}
}

// WithAWSConfig reuses an already loaded AWS SDK configuration. When omitted,
// New loads AWS configuration from the same environment variables as the
// standalone server.
func WithAWSConfig(cfg aws.Config) Option {
	return func(opts *options) {
		opts.awsConfig = &cfg
	}
}

// Adapter owns the HTTP handler and background resources used by the SQS
// backend. It implements http.Handler directly.
type Adapter struct {
	handler   http.Handler
	backend   *sqsbackend.SQSBackend
	broker    *sqsbackend.PubSubBroker
	scheduler *scheduler.Scheduler

	closeOnce sync.Once
	closeErr  error
}

// New loads the standard server configuration from the environment, ensures
// the configured DynamoDB table exists, constructs the SQS backend, starts its
// background scheduler, and returns the existing HTTP router.
func New(ctx context.Context, option ...Option) (*Adapter, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	opts := options{logger: slog.Default()}
	for _, apply := range option {
		apply(&opts)
	}
	if opts.logger == nil {
		opts.logger = slog.Default()
	}

	cfg := server.LoadConfig()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate SQS Lambda configuration: %w", err)
	}

	awsCfg, err := resolveAWSConfig(ctx, cfg, opts.awsConfig)
	if err != nil {
		return nil, fmt.Errorf("configure AWS: %w", err)
	}

	store := state.NewDynamoDBStore(dynamodb.NewFromConfig(awsCfg), cfg.DynamoDBTable)
	if err := store.EnsureTable(ctx); err != nil {
		return nil, fmt.Errorf("ensure DynamoDB table %q: %w", cfg.DynamoDBTable, err)
	}

	backend := sqsbackend.New(awssqs.NewFromConfig(awsCfg), store, cfg.SQSQueuePrefix, cfg.UseFIFO)
	backend.SetLogger(opts.logger)

	metrics.Init(core.OJSVersion, "sqs")

	sched := scheduler.New(backend, opts.logger)
	sched.Start()
	broker := sqsbackend.NewPubSubBroker()

	return &Adapter{
		handler:   server.NewRouterWithRealtime(backend, opts.logger, cfg, broker, broker),
		backend:   backend,
		broker:    broker,
		scheduler: sched,
	}, nil
}

func resolveAWSConfig(ctx context.Context, cfg server.Config, provided *aws.Config) (aws.Config, error) {
	if provided != nil {
		return *provided, nil
	}

	loadOptions := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.AWSRegion),
	}
	endpoint := cfg.AWSEndpointURL
	if cfg.LocalStackEndpoint != "" {
		endpoint = cfg.LocalStackEndpoint
	}
	if endpoint != "" {
		loadOptions = append(loadOptions, config.WithBaseEndpoint(endpoint))
	}
	if cfg.LocalStackEndpoint != "" {
		loadOptions = append(loadOptions,
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "test")),
		)
	}
	return config.LoadDefaultConfig(ctx, loadOptions...)
}

// Handler returns the configured OJS HTTP handler.
func (a *Adapter) Handler() http.Handler {
	return a.handler
}

// ServeHTTP implements http.Handler.
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.handler.ServeHTTP(w, r)
}

// Close stops background work and releases backend resources. It is safe to
// call more than once.
func (a *Adapter) Close() error {
	a.closeOnce.Do(func() {
		a.scheduler.Stop()
		a.closeErr = errors.Join(a.broker.Close(), a.backend.Close())
	})
	return a.closeErr
}
