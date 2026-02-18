package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"google.golang.org/grpc"

	ojsotel "github.com/openjobspec/ojs-go-backend-common/otel"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	ojsgrpc "github.com/openjobspec/ojs-backend-sqs/internal/grpc"
	"github.com/openjobspec/ojs-backend-sqs/internal/metrics"
	"github.com/openjobspec/ojs-backend-sqs/internal/scheduler"
	"github.com/openjobspec/ojs-backend-sqs/internal/server"
	sqsbackend "github.com/openjobspec/ojs-backend-sqs/internal/sqs"
	"github.com/openjobspec/ojs-backend-sqs/internal/state"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg := server.LoadConfig()
	if cfg.APIKey == "" && !cfg.AllowInsecureNoAuth {
		logger.Error("refusing to start without API authentication", "hint", "set OJS_API_KEY or OJS_ALLOW_INSECURE_NO_AUTH=true for local development")
		os.Exit(1)
	}
	if cfg.AllowInsecureNoAuth {
		slog.Warn("⚠️  RUNNING WITHOUT AUTHENTICATION — this is intended for local development only. Set OJS_API_KEY for any shared or production environment.")
	}

	// Initialize OpenTelemetry (opt-in via OJS_OTEL_ENABLED or OTEL_EXPORTER_OTLP_ENDPOINT)
	otelEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if ep := os.Getenv("OJS_OTEL_ENDPOINT"); ep != "" {
		otelEndpoint = ep
	}
	otelShutdown, otelErr := ojsotel.Init(context.Background(), ojsotel.Config{
		ServiceName:    "ojs-backend-sqs",
		ServiceVersion: core.OJSVersion,
		Enabled:        os.Getenv("OJS_OTEL_ENABLED") == "true" || otelEndpoint != "",
		Endpoint:       otelEndpoint,
	})
	if otelErr != nil {
		logger.Error("failed to initialize OpenTelemetry", "error", otelErr)
		os.Exit(1)
	}
	defer func() { _ = otelShutdown(context.Background()) }()

	// Configure AWS SDK
	awsCfg, err := buildAWSConfig(cfg)
	if err != nil {
		logger.Error("failed to configure AWS", "error", err)
		os.Exit(1)
	}

	// Create AWS clients
	sqsClient := sqs.NewFromConfig(awsCfg)
	dynamoClient := dynamodb.NewFromConfig(awsCfg)

	// Create DynamoDB state store
	store := state.NewDynamoDBStore(dynamoClient, cfg.DynamoDBTable)
	if err := store.EnsureTable(context.Background()); err != nil {
		logger.Error("failed to ensure DynamoDB table", "error", err)
		os.Exit(1)
	}
	logger.Info("DynamoDB state store ready", "table", cfg.DynamoDBTable)

	// Create SQS backend
	backend := sqsbackend.New(sqsClient, store, cfg.SQSQueuePrefix, cfg.UseFIFO)
	backend.SetLogger(logger)
	defer backend.Close()

	// Initialize Prometheus server info metric
	metrics.Init(core.OJSVersion, "sqs")

	logger.Info("SQS backend ready",
		"prefix", cfg.SQSQueuePrefix,
		"fifo", cfg.UseFIFO,
		"region", cfg.AWSRegion,
	)

	// Start background scheduler
	sched := scheduler.New(backend, logger)
	sched.Start()
	defer sched.Stop()

	// Initialize real-time Pub/Sub broker
	broker := sqsbackend.NewPubSubBroker()
	defer broker.Close()

	// Create HTTP server with real-time support
	router := server.NewRouterWithRealtime(backend, logger, cfg, broker, broker)
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	// Start server
	go func() {
		logger.Info("OJS server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Start gRPC server
	grpcServer := grpc.NewServer()
	ojsgrpc.Register(grpcServer, backend)

	go func() {
		lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
		if err != nil {
			logger.Error("failed to listen for gRPC", "port", cfg.GRPCPort, "error", err)
			os.Exit(1)
		}
		logger.Info("OJS gRPC server listening", "port", cfg.GRPCPort)
		if err := grpcServer.Serve(lis); err != nil {
			logger.Error("gRPC server error", "error", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("shutting down...")
	sched.Stop()
	grpcServer.GracefulStop()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("server shutdown error", "error", err)
	}

	logger.Info("server stopped")
}

func buildAWSConfig(cfg server.Config) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.AWSRegion),
	}

	// For LocalStack or custom endpoints
	if cfg.AWSEndpointURL != "" {
		customResolver := aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:               cfg.AWSEndpointURL,
					HostnameImmutable: true,
					PartitionID:       "aws",
				}, nil
			},
		)
		opts = append(opts,
			config.WithEndpointResolverWithOptions(customResolver),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "test")),
		)
	}

	return config.LoadDefaultConfig(context.Background(), opts...)
}
