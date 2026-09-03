# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-09-02

### Added
- Public `lambda` package for constructing the supported SQS HTTP backend in
  serverless deployments without importing internal packages.

### Changed
- Upgraded gRPC, OpenTelemetry, Chi, and Go networking dependencies to patched
  releases and raised the minimum Go version to 1.25.
- Migrated native linting to pinned golangci-lint v2.
- Added fenced delivery claims, durable outboxes/effects, OJS-managed retries,
  restart-safe workflows, and multi-replica cron ownership.
- Custom AWS endpoints preserve the normal credential chain; static test
  credentials now require the explicit LocalStack endpoint.
- WebSocket event IDs retain counters beyond four digits.
- Aligned release dependencies and hardened LocalStack CI, release artifacts,
  checksums, and provenance for the coordinated 0.5.0 train.

## [0.4.1] - 2026-04-21

### Fixed
- Added deterministic scheduler shutdown and gRPC marshaling error handling.
- Logged previously ignored state-store and SQS errors.

## [0.4.0] - 2026-04-20

### Added
- Initial SQS-backed OpenJobSpec server with DynamoDB state store.
- AWS LocalStack support for local development.
- Terraform infrastructure definitions for production AWS deployment.
- FIFO queue support via `SQS_USE_FIFO` configuration.
- Full OJS conformance support (levels 0–4).
- Docker Compose setup with LocalStack and Redis.
- Project governance files (`CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`).
