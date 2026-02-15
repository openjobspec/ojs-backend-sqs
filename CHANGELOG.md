# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Initial SQS-backed OpenJobSpec server with DynamoDB state store.
- AWS LocalStack support for local development.
- Terraform infrastructure definitions for production AWS deployment.
- FIFO queue support via `SQS_USE_FIFO` configuration.
- Full OJS conformance support (levels 0–4).
- Docker Compose setup with LocalStack and Redis.
- Project governance files (`CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`).
