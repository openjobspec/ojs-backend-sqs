# Contributing to ojs-backend-sqs

Thank you for your interest in contributing to the SQS backend for Open Job Spec!

## Prerequisites

- Go 1.22+
- Docker (for LocalStack and Redis)
- AWS CLI (optional, for manual testing)

## Local Development Setup

```bash
# Clone the repository
git clone https://github.com/openjobspec/ojs-backend-sqs.git
cd ojs-backend-sqs

# Start dependencies (LocalStack + Redis)
make docker-up

# Build
make build

# Run tests
make test

# Run linter
make lint
```

### Running with LocalStack

The SQS backend uses [LocalStack](https://localstack.cloud/) for local development, which emulates SQS and DynamoDB:

```bash
# Start LocalStack + Redis + OJS server
make docker-up

# Or run the server directly (requires LocalStack + Redis running)
make run
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AWS_REGION` | `us-east-1` | AWS region |
| `AWS_ENDPOINT_URL` | `http://localhost:4566` | LocalStack endpoint |
| `AWS_ACCESS_KEY_ID` | `test` | AWS access key (LocalStack) |
| `AWS_SECRET_ACCESS_KEY` | `test` | AWS secret key (LocalStack) |
| `DYNAMODB_TABLE` | `ojs-jobs` | DynamoDB table for job state |
| `SQS_QUEUE_PREFIX` | `ojs` | Prefix for SQS queue names |
| `REDIS_URL` | `redis://localhost:6379` | Redis URL for state caching |

## Architecture

The project uses a three-layer architecture:

- **`internal/api/`** — HTTP and gRPC handlers (chi router)
- **`internal/core/`** — Business logic interfaces and types
- **`internal/sqs/`** — SQS + DynamoDB backend implementation
- **`internal/scheduler/`** — Background job promotion and reaping

See the [README](README.md) for the full architecture diagram.

## Development Workflow

1. Fork the repository
2. Create a feature branch from `main`: `git checkout -b feat/my-feature`
3. Make your changes
4. Run tests and linter: `make test && make lint`
5. Commit using [Conventional Commits](https://www.conventionalcommits.org/):
   - `feat:` for new features
   - `fix:` for bug fixes
   - `docs:` for documentation changes
   - `refactor:` for code refactoring
   - `test:` for test additions/changes
   - `chore:` for maintenance tasks
6. Push to your fork and open a Pull Request

## Code Guidelines

- Follow existing code patterns and conventions
- Add tests for new functionality
- Use `log/slog` for structured logging (not `log.Printf`)
- Handle errors explicitly; do not silently ignore them
- Keep SQS and DynamoDB operations atomic where possible

## Testing

```bash
make test           # Run all tests with race detector
make lint           # Run golangci-lint
make conformance    # Run OJS conformance tests (requires running server)
```

## AWS Deployment

For real AWS deployment, see the Terraform configuration in `deploy/terraform/`.

## Pull Request Guidelines

1. Create a topic branch from `main`.
2. Keep changes scoped and include tests for behavior changes.
3. Ensure build and tests pass locally.
4. If a change is breaking, call it out explicitly and update `CHANGELOG.md`.
5. Open a PR with a clear description of the change and motivation.

## Reporting Issues

- Use the [bug report template](.github/ISSUE_TEMPLATE/bug_report.yml) for bugs
- Use the [feature request template](.github/ISSUE_TEMPLATE/feature_request.yml) for new ideas

## License

By contributing, you agree that your contributions will be licensed under the Apache-2.0 License.
