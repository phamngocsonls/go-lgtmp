BINARY      := server
BUILD_DIR   := bin
IMAGE_NAME  := go-lgtmp
IMAGE_TAG   ?= dev
REGISTRY    ?= ghcr.io/go-lgtmp

.PHONY: run infra infra-down infra-lgtm infra-lgtm-down build test lint verify load docker-build docker-push clean help

## run: Run the service locally (requires LGTMP stack — see 'make infra')
run:
	go run ./cmd/server

## infra: Start local dev infrastructure (Postgres, Redis, Alloy, Grafana stack)
infra:
	docker compose up -d

## infra-down: Stop and remove local dev infrastructure
infra-down:
	docker compose down -v

## infra-lgtm: Start simple stack (grafana/otel-lgtm single image — OTel Collector, Prometheus, no Alloy/Mimir)
infra-lgtm:
	docker compose -f docker-compose.lgtm.yml up -d

## infra-lgtm-down: Stop simple stack
infra-lgtm-down:
	docker compose -f docker-compose.lgtm.yml down -v

## build: Compile the binary to ./bin/server
build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o $(BUILD_DIR)/$(BINARY) ./cmd/server
	@echo "Built: $(BUILD_DIR)/$(BINARY)"

## test: Run all tests with race detector
test:
	go test -race -count=1 -cover ./...

## lint: Run golangci-lint (install: https://golangci-lint.run/usage/install/)
lint:
	golangci-lint run ./...

## verify: Verify go module integrity (go mod tidy skipped — OTel SDK test dep conflict)
verify:
	go mod verify
	go build ./...

## load: Generate demo traffic (requires running service on :8080)
load:
	@echo "Sending traffic to http://localhost:8080 — Ctrl+C to stop"
	@while true; do \
		curl -sf http://localhost:8080/ping              > /dev/null; \
		curl -sf http://localhost:8080/rolldice          > /dev/null; \
		curl -sf http://localhost:8080/rolldice          > /dev/null; \
		curl -sf http://localhost:8080/rolldice          > /dev/null; \
		n=$$((RANDOM % 35 + 5)); \
		curl -sf "http://localhost:8080/fibonacci?n=$$n" > /dev/null; \
		curl -sf http://localhost:8080/db/users          > /dev/null 2>&1 || true; \
		curl -sf http://localhost:8080/cache/users/1     > /dev/null 2>&1 || true; \
		sleep 0.3; \
	done

## docker-build: Build the Docker image
docker-build:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .

## docker-push: Push image to registry (set REGISTRY env var)
docker-push: docker-build
	docker tag $(IMAGE_NAME):$(IMAGE_TAG) $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
	docker push $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## help: Show this help
help:
	@grep -E '^## ' Makefile | sed 's/## //'
