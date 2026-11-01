# Ride-hailing dispatch — common commands. Run `make help`.
SHELL := /bin/bash
COMPOSE := docker compose
# testcontainers + Colima: use the socket path as seen from inside the VM.
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE ?= /var/run/docker.sock

.PHONY: help up down clean ps logs build test test-int test-e2e test-all vet trace-ride trace-logs demo postman postman-gen proto openapi-lint grpc-offers grpc-location rebuild-geo replay-dlq psql

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

up: ## Build and start the whole stack (waits until healthy)
	$(COMPOSE) up -d --build --wait

down: ## Stop the stack (keeps data)
	$(COMPOSE) down

clean: ## Stop the stack and delete all data volumes
	$(COMPOSE) down -v --remove-orphans

ps: ## Show container status
	$(COMPOSE) ps

logs: ## Follow logs of the three Go services
	$(COMPOSE) logs -f ride-service driver-service dispatch-service

build: ## Compile all Go code
	go build ./...

vet: ## go vet (all build tags)
	go vet ./... && go vet -tags integration ./... && go vet -tags e2e ./test/e2e/

test: ## Unit tests (no Docker needed)
	go test -count=1 ./...

test-int: ## Unit + integration tests (testcontainers: Postgres, Redis)
	go test -count=1 -tags integration ./...

test-e2e: ## End-to-end tests against the running stack (run `make up` first; ~40 s)
	go test -count=1 -tags e2e -v ./test/e2e/

test-all: test-int test-e2e ## Everything

trace-ride: ## Show what happened to a ride from the logs: make trace-ride RIDE=<rideId>
	@./scripts/trace-ride.sh $(RIDE)

trace-logs: ## Every log line of a trace incl. HTTP bodies and message values: make trace-logs TRACE=<traceId>
	@./scripts/trace-logs.sh $(TRACE)

demo: ## Scripted walkthrough with every request/response logged (COMPACT=1 for one-line JSON)
	@./scripts/demo.sh

postman: ## Run the Postman collection from the CLI with newman (needs Node; ~45 s incl. timeout scenario)
	npx -y newman@6 run postman/ridehail.postman_collection.json

postman-gen: ## Regenerate the Postman collection from scripts/gen-postman.py
	python3 scripts/gen-postman.py postman/ridehail.postman_collection.json

proto: ## Regenerate gRPC code from api/proto (needs protoc, protoc-gen-go, protoc-gen-go-grpc)
	PATH="$$PATH:$$(go env GOPATH)/bin" protoc -I api/proto --go_out=. --go_opt=module=ridehail \
	  --go-grpc_out=. --go-grpc_opt=module=ridehail $$(find api/proto -name '*.proto')

openapi-lint: ## Lint the REST contract api/openapi.yaml (needs Node)
	npx -y @redocly/cli@1.34.5 lint --config api/redocly.yaml api/openapi.yaml

GRPCURL := grpcurl -plaintext -import-path api/proto

grpc-offers: ## API v2: watch a driver's offers as a gRPC stream (Ctrl+C to stop): make grpc-offers DRIVER=D001
	$(GRPCURL) -proto ridehail/dispatch/v2/offers.proto -d '{"driverId":"$(or $(DRIVER),D001)"}' \
	  localhost:9090 ridehail.dispatch.v2.OfferService/WatchOffers

grpc-location: ## API v2: stream two location updates for a driver: make grpc-location DRIVER=D010 (moves it ~10 m)
	$(GRPCURL) -proto ridehail/driver/v2/location.proto -d '{"driverId":"$(or $(DRIVER),D010)","latitude":10.7969,"longitude":106.7010,"seq":1} {"driverId":"$(or $(DRIVER),D010)","latitude":10.7969,"longitude":106.7009,"seq":2}' \
	  localhost:9090 ridehail.driver.v2.DriverLocationService/StreamLocations

rebuild-geo: ## Rebuild the Redis GEO index from driver-service
	curl -s -X POST localhost:8083/internal/geo/rebuild; echo

replay-dlq: ## Re-publish dead-lettered messages: make replay-dlq TOPIC=ride.commands.v1
	@./scripts/replay-dlq.sh $(TOPIC)

psql: ## Open psql on a service database: make psql DB=dispatch_db
	$(COMPOSE) exec postgres psql -U postgres -d $(or $(DB),ride_db)
