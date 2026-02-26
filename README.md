# ojs-backend-sqs

[![CI](https://github.com/openjobspec/ojs-backend-sqs/actions/workflows/ci.yml/badge.svg)](https://github.com/openjobspec/ojs-backend-sqs/actions/workflows/ci.yml)
![Conformance](https://github.com/openjobspec/ojs-backend-sqs/raw/main/.github/badges/conformance.svg)
[![Go Report Card](https://goreportcard.com/badge/github.com/openjobspec/ojs-backend-sqs)](https://goreportcard.com/report/github.com/openjobspec/ojs-backend-sqs)

An [Open Job Spec (OJS)](https://github.com/openjobspec/openjobspec) backend implementation using **AWS SQS** for message transport and **DynamoDB** for job state tracking.

## Architecture

```
┌─────────────┐     ┌──────────────┐     ┌─────────────┐
│  OJS HTTP   │────▶│  SQS Backend │────▶│   AWS SQS   │
│   API       │     │   (Go)       │     │  (queues)   │
└─────────────┘     └──────┬───────┘     └─────────────┘
                           │
                           ▼
                    ┌──────────────┐
                    │  DynamoDB    │
                    │ (state store)│
                    └──────────────┘
```

This backend uses a **hybrid architecture**:
- **AWS SQS** handles message transport (enqueue, dequeue, visibility timeout, dead letter)
- **DynamoDB** tracks job state, metadata, workflows, cron definitions, and unique constraints

The same `core.Backend` interface and HTTP API handlers from the [Redis backend](../ojs-backend-redis/) are reused. Only the storage layer changes.

## OJS-to-SQS Concept Mapping

| OJS Concept | SQS Implementation |
|---|---|
| OJS queue | One SQS queue per OJS queue |
| Job enqueue | `SendMessage` |
| Batch enqueue | `SendMessageBatch` (max 10 per call, auto-chunked) |
| Job fetch | `ReceiveMessage` with short polling |
| Job ack | `DeleteMessage` |
| Job nack (requeue) | `ChangeMessageVisibility` to 0 |
| Job nack (exhausted) | `DeleteMessage` + DLQ in state store |
| Visibility timeout | SQS `VisibilityTimeout` (native) |
| Heartbeat | `ChangeMessageVisibility` (extend) |
| Priority | Separate SQS queues per priority tier (via state store) |
| Scheduled jobs (≤15min) | SQS `DelaySeconds` |
| Scheduled jobs (>15min) | State store + scheduler goroutine |
| Dead letter | SQS native DLQ + state store tracking |
| Job state | DynamoDB (SQS is opaque once in-flight) |
| Unique jobs | DynamoDB conditional writes |
| Workflows | DynamoDB state tracking |
| Cron | DynamoDB + background scheduler |

## Quick Start

### Local Development (LocalStack)

```bash
# Start LocalStack + OJS server
make docker-up

# Verify health
curl http://localhost:8080/ojs/v1/health

# Enqueue a job
curl -X POST http://localhost:8080/ojs/v1/jobs \
  -H "Content-Type: application/openjobspec+json" \
  -d '{"type": "email.send", "args": ["user@example.com", "Hello!"]}'

# Fetch a job
curl -X POST http://localhost:8080/ojs/v1/workers/fetch \
  -H "Content-Type: application/openjobspec+json" \
  -d '{"queues": ["default"], "worker_id": "worker-1"}'

# Stop
make docker-down
```

### Run Without Docker

```bash
# Start LocalStack separately
docker run -d --name localstack -p 4566:4566 \
  -e SERVICES=sqs,dynamodb \
  -e DEFAULT_REGION=us-east-1 \
  localstack/localstack:3

# Build and run
make run
```

## Configuration

| Environment Variable | Default | Description |
|---|---|---|
| `OJS_PORT` | `8080` | HTTP server port |
| `AWS_REGION` | `us-east-1` | AWS region |
| `AWS_ENDPOINT_URL` | _(empty)_ | Custom endpoint (for LocalStack: `http://localhost:4566`) |
| `DYNAMODB_TABLE` | `ojs-jobs` | DynamoDB table name |
| `SQS_QUEUE_PREFIX` | `ojs` | Prefix for SQS queue names |
| `SQS_USE_FIFO` | `false` | Use FIFO queues (exactly-once, strict ordering) |

## Build & Test

```bash
make build          # Build server binary to bin/ojs-server
make test           # go test ./... -race -cover
make lint           # golangci-lint run ./...
make run            # Build and run (needs LocalStack or real AWS)
make docker-up      # Start server + LocalStack via Docker Compose
make docker-down    # Stop Docker Compose
```

### Development with Hot Reload

```bash
make dev            # Local hot reload (requires air)
make docker-dev     # Docker Compose with hot reload
```

## Conformance

```bash
make conformance              # Run all conformance levels
make conformance-level-0      # Run specific level (0-4)
```

## AWS Deployment

Terraform configuration is provided for AWS provisioning:

```bash
cd terraform/
terraform init
terraform plan
terraform apply
```

This creates:
- DynamoDB table with GSIs (pay-per-request billing)
- SQS queues with DLQs for each configured OJS queue
- TTL enabled for automatic cleanup

## Trade-offs vs. Redis/Postgres Backends

### Strengths
- **Fully managed** — no infrastructure to maintain
- **Native DLQ** — SQS dead letter queues with redrive policies
- **Native visibility timeout** — first-class support, no polling needed
- **Auto-scaling** — Standard queues scale to nearly unlimited throughput
- **High durability** — multi-AZ replication by default
- **Pay-per-use** — no idle cost for quiet queues

### Weaknesses
- **Higher latency** — ~20ms enqueue vs ~1ms for Redis
- **256KB message limit** — large payloads need S3 offloading
- **15-minute max delay** — longer scheduling requires state store
- **Max 10 messages per receive** — multiple API calls for large fetches
- **No transactional enqueue** — can't atomically enqueue with app data
- **External state store required** — SQS is opaque for job lifecycle tracking
- **FIFO throughput ceiling** — 3,000 msg/s with batching
- **AWS vendor lock-in** — requires AWS infrastructure

## Performance Targets

| Operation | Target |
|---|---|
| Enqueue | < 20ms p99 |
| Dequeue | < 50ms p99 (excludes wait time) |
| Throughput (Standard) | Nearly unlimited |
| Throughput (FIFO) | 3,000 msg/s with batching |
| Connected workers | Up to 10,000 |

## Conformance Notes

| Level | Support | Notes |
|---|---|---|
| 0 | Full | SQS is a natural fit for basic queue operations |
| 1 | Full | SQS native visibility timeout + DLQ |
| 2 | Partial | `DelaySeconds` for < 15min; state store for longer delays and cron |
| 3 | Full | Workflow step tracking via DynamoDB |
| 4 | Full | Unique jobs via DynamoDB; priority via queue tiers; bulk via `SendMessageBatch` |

## SQS Queue Naming Convention

```
ojs-{queue_name}                 -- standard queue
ojs-{queue_name}.fifo            -- FIFO queue variant
ojs-{queue_name}-dlq             -- dead letter queue
ojs-{queue_name}-dlq.fifo        -- FIFO dead letter queue
```

## Large Payload Pattern

SQS messages have a 256KB size limit. For larger payloads, store the data in S3 and pass a reference:

```json
{
  "type": "video.transcode",
  "args": [{"s3_uri": "s3://my-bucket/jobs/payload-12345.json"}]
}
```

The worker reads the full payload from S3 at processing time. This pattern is documented as an OJS extension.

## Observability

### OpenTelemetry

The server supports distributed tracing via OpenTelemetry. Set the following environment variable to enable:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
```

Traces are exported in OTLP format over gRPC. Compatible with Jaeger, Zipkin, Grafana Tempo, and any OTLP-compatible collector.

You can also use the legacy env vars `OJS_OTEL_ENABLED=true` and `OJS_OTEL_ENDPOINT` for explicit control.

## Production Deployment Notes

- **Rate limiting**: This server does not enforce request rate limits. Place a reverse proxy (e.g., Nginx, Envoy, or a cloud load balancer) in front of the server to add rate limiting in production.
- **Authentication**: Set `OJS_API_KEY` to require Bearer token auth on all endpoints. For local-only testing, set `OJS_ALLOW_INSECURE_NO_AUTH=true`.
- **TLS**: Terminate TLS at a reverse proxy or load balancer rather than at the application level.

## License

Apache-2.0 — see [LICENSE](LICENSE).

