# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.0](https://github.com/openjobspec/ojs-backend-sqs/compare/v0.1.0...v0.2.0) (2026-02-28)


### Features

* add FIFO queue support ([a9abd73](https://github.com/openjobspec/ojs-backend-sqs/commit/a9abd735381a5ddc7df28f5b65c18fd775d4c045))
* add health check endpoint ([30b9ad7](https://github.com/openjobspec/ojs-backend-sqs/commit/30b9ad717ce33ddb725a2974289a9b6b807e1a63))
* add health check endpoint ([a207604](https://github.com/openjobspec/ojs-backend-sqs/commit/a2076046917f5a21ffab9c4d91e5b85b44d7aa69))


### Bug Fixes

* correct status transition validation for retryable state ([94d5169](https://github.com/openjobspec/ojs-backend-sqs/commit/94d5169df918ae6f3a8795871213321127efa814))
* correct visibility timeout extension ([0ea113f](https://github.com/openjobspec/ojs-backend-sqs/commit/0ea113fb392fb262a9a70bd27882f70f9e3ce9d9))
* handle connection pool exhaustion gracefully ([81ea292](https://github.com/openjobspec/ojs-backend-sqs/commit/81ea292f04b24523e9fe4029c6ca2f3a42baa010))

## [Unreleased]

### Added
- Initial SQS-backed OpenJobSpec server with DynamoDB state store.
- AWS LocalStack support for local development.
- Terraform infrastructure definitions for production AWS deployment.
- FIFO queue support via `SQS_USE_FIFO` configuration.
- Full OJS conformance support (levels 0–4).
- Docker Compose setup with LocalStack and Redis.
- Project governance files (`CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`).
