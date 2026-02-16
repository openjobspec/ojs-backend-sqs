.PHONY: build run test lint clean docker-up docker-down conformance conformance-all dev docker-dev

CONFORMANCE_RUNNER = ../ojs-conformance/runner/http
CONFORMANCE_SUITES = ../../suites
OJS_URL ?= http://localhost:8080
AWS_ENDPOINT_URL ?= http://localhost:4566
REDIS_URL ?= redis://localhost:6379

build:
	go build -o bin/ojs-server ./cmd/ojs-server

run: build
	AWS_REGION=us-east-1 \
	AWS_ENDPOINT_URL=$(AWS_ENDPOINT_URL) \
	AWS_ACCESS_KEY_ID=test \
	AWS_SECRET_ACCESS_KEY=test \
	DYNAMODB_TABLE=ojs-jobs \
	SQS_QUEUE_PREFIX=ojs \
	./bin/ojs-server

test:
	go test ./... -race -cover

lint:
	golangci-lint run ./...

lint-vet:
	go vet ./...

clean:
	rm -rf bin/

# Docker (LocalStack + Redis + OJS server)
docker-up:
	docker compose -f docker/docker-compose.yml up --build -d

docker-down:
	docker compose -f docker/docker-compose.yml down -v

# Conformance tests (require running server + LocalStack + Redis for test isolation)
conformance: conformance-all

conformance-all:
	cd $(CONFORMANCE_RUNNER) && go run . -url $(OJS_URL) -suites $(CONFORMANCE_SUITES) -redis $(REDIS_URL)

conformance-level-0:
	cd $(CONFORMANCE_RUNNER) && go run . -url $(OJS_URL) -suites $(CONFORMANCE_SUITES) -level 0 -redis $(REDIS_URL)

conformance-level-1:
	cd $(CONFORMANCE_RUNNER) && go run . -url $(OJS_URL) -suites $(CONFORMANCE_SUITES) -level 1 -redis $(REDIS_URL)

conformance-level-2:
	cd $(CONFORMANCE_RUNNER) && go run . -url $(OJS_URL) -suites $(CONFORMANCE_SUITES) -level 2 -redis $(REDIS_URL)

conformance-level-3:
	cd $(CONFORMANCE_RUNNER) && go run . -url $(OJS_URL) -suites $(CONFORMANCE_SUITES) -level 3 -redis $(REDIS_URL)

conformance-level-4:
	cd $(CONFORMANCE_RUNNER) && go run . -url $(OJS_URL) -suites $(CONFORMANCE_SUITES) -level 4 -redis $(REDIS_URL)

# Development with hot reload
dev:
	air -c .air.toml

docker-dev:
	docker compose -f docker/docker-compose.yml --profile dev up --build
