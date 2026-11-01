# Local Setup & Run Guide

This guide takes a Mac from zero to the full system running in Docker, with tests passing. **No Docker Desktop:** the Docker engine runs inside **Colima**, a small Linux VM, and you use the normal `docker` CLI. **lazydocker** is a terminal UI for watching the containers.

Tested on macOS (Apple Silicon) with Colima 0.x / Docker 29.5 / Compose 5.6 / Go 1.27.3.

---

## 1. Install the tools (one time)

```bash
brew install colima docker docker-compose docker-buildx lazydocker go jq grpcurl
```

| Tool | Why |
|---|---|
| `colima` | Runs the Docker engine in a lightweight VM (replaces Docker Desktop) |
| `docker` | Docker CLI |
| `docker-compose`, `docker-buildx` | CLI plugins for `docker compose` and BuildKit builds |
| `lazydocker` | Terminal UI for containers, logs and stats |
| `go` | Build and run tests on the host (Go ≥ 1.27) |
| `jq` | Used by `make trace-ride` and `make demo` |
| `grpcurl` | Optional: call the gRPC API v2 from the terminal (`make grpc-offers`, `make grpc-location`) |

Only to change the `.proto` files (generated code is committed): `brew install protobuf` and `go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12 google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1`, then `make proto`.

### Let the Docker CLI find the plugins

Homebrew installs Compose and Buildx as plugins in a folder Docker does not search by default. Add `cliPluginsExtraDirs` to `~/.docker/config.json`, keeping whatever is already in the file:

```json
{
  "cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]
}
```

Check:

```bash
docker compose version   # Docker Compose version 5.x
docker buildx version
```

## 2. Start the Docker engine (Colima)

```bash
colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --vz-rosetta
```

* `--vm-type vz --vz-rosetta`: Apple's virtualization framework, the fastest option on Apple Silicon. On Intel Macs, drop these two flags.
* 4 CPU / 8 GB is comfortable for the 10 containers. 2 CPU / 6 GB also works, but starts more slowly.

Point the CLI at Colima. Pick **one** of these:

```bash
docker context use colima
# or, in ~/.zshrc:
export DOCKER_HOST=unix://$HOME/.colima/docker.sock
```

Check:

```bash
colima status
docker info --format '{{.ServerVersion}} {{.OperatingSystem}}'   # e.g. 29.5.2 Ubuntu 24.04
docker run --rm hello-world
```

Useful Colima commands:

| Command | Does |
|---|---|
| `colima stop` | Stop the VM and free its CPU/RAM (containers stop too) |
| `colima start` | Start it again with the previous settings |
| `colima status` / `colima list` | Is it running, and with which resources |
| `brew services start colima` | Start Colima automatically at login |
| `colima delete` | Remove the VM and everything in it (images, volumes) |

## 3. Run the system

```bash
cd coding-challenge-3/kvp
make up
```

`make up` runs `docker compose up -d --build --wait`. It builds the three Go services (one shared [Dockerfile](../Dockerfile)), starts the infrastructure, and returns once every container is **healthy**. The first build downloads images and Go modules and takes a few minutes; later runs take seconds.

Startup order (Compose health checks enforce it):

1. **postgres** creates `ride_db`, `driver_db` and `dispatch_db`, each with its own user ([init.sql](../deploy/postgres/init.sql)).
2. **redpanda** and **redis** start.
3. Each **Go service** connects, **applies its own SQL migrations** (embedded in the binary, guarded by an advisory lock), creates the Redpanda topics it needs, then starts its HTTP server, outbox relay and consumers. The migrations also **seed** passengers `P001–P010` and drivers `D001–D020`. D001–D010 are `AVAILABLE`, ~220 m apart going north from Ben Thanh, District 1; D011–D020 are `OFFLINE`.
4. **dispatch-service** fills the Redis GEO index from driver-service on first start.
5. **gateway** (nginx) starts last and exposes the API on port 8080.

### What is running

| Container | Purpose | URL / port on your Mac |
|---|---|---|
| `gateway` | **Public API**: REST v1 routed by path; gRPC v2 routed by service | **http://localhost:8080** (REST), **localhost:9090** (gRPC) |
| `ride-service` | Rides, passengers, ride state machine | localhost:8081 |
| `driver-service` | Driver status/location, atomic reservation; gRPC `DriverLocationService` | localhost:8082 (also `/internal/*` fixtures) |
| `dispatch-service` | Saga, matching, offers, timeouts, GEO projection; gRPC `OfferService` | localhost:8083 |
| `postgres` | 3 databases (user `postgres` / `postgres`) | localhost:5432 |
| `redis` | GEO index | localhost:6379 |
| `redpanda` | Kafka API broker | localhost:19092 |
| `swagger-ui` | REST API documentation ([api/openapi.yaml](../api/openapi.yaml)), behind the gateway | **http://localhost:8080/docs/** |
| `redpanda-console` | Browse topics, messages, consumer groups | **http://localhost:8088** |
| `lgtm` | Grafana + Loki + Tempo + Prometheus + OTel Collector | **http://localhost:3000** (no login) |

```bash
make ps      # status of every container
make logs    # follow the three Go services' logs (plain text, key=value)
make down    # stop everything, keep the data
make clean   # stop everything and delete all data (fresh seed on next `make up`)
```

## 4. Try it

### Scripted demo

```bash
make demo
```

It creates three demo drivers and walks through: request a ride (and repeat the same request), reject → rematch → accept twice → complete, an invalid transition, and two rides racing for one driver. It ends with the ride's timeline from the logs. It can be run any number of times.

Every HTTP call is logged with its request (method, URL, headers, body) and response (status, notable headers such as `Idempotent-Replayed`, body):

```
→ POST http://localhost:8080/rides
  Idempotency-Key: demo-27456
  {"passengerId":"P001","pickup":{"latitude":12.80,"longitude":106.7700},"destination":{…}}
← 201 Created  [Idempotent-Replayed: true]
  {"rideId":"01a19673-…","status":"REQUESTED","assignedDriverId":null,…}
```

JSON is pretty-printed; `COMPACT=1 make demo` prints one line per body.

### Swagger UI (OpenAPI)

Open **http://localhost:8080/docs/**. It renders [api/openapi.yaml](../api/openapi.yaml), the REST contract: every endpoint, schema and error `code`. **Try it out** sends real requests to the running stack. The raw file is also served at http://localhost:8080/openapi.yaml, so any tool can import it (Postman → Import → URL, Insomnia, Bruno, client generators). `make openapi-lint` lints it.

### Postman

Import [postman/ridehail.postman_collection.json](../postman/ridehail.postman_collection.json) (Postman → **Import** → select the file). It has a happy-path scenario and one folder per acceptance criterion, with one or two checks per request:

| Folder | What it shows | Requests |
|---|---|---|
| Scenario · Happy path | Request → nearest driver gets the offer → accept → driver `BUSY` → complete → driver `AVAILABLE`; history and dispatch view | 9 |
| AC-01 · Create ride | `REQUESTED` with an id; invalid coordinates, unknown passenger, missing `Idempotency-Key` rejected | 5 |
| AC-02 · AC-03 · Driver availability and location | `OFFLINE ⇄ AVAILABLE`; `BUSY` cannot be set; latest location stored; invalid location rejected | 5 |
| AC-04 · Matching | Nearest wins; a tie goes to the smaller `driverId`; a driver outside 2 km gets nothing | 4 |
| AC-05 · AC-07 · Reject → rematch → exclusion | Only the offered driver can answer; rejection → next driver; the rejecting driver never gets the ride again; nobody left → `MATCHING` | 9 |
| AC-05 · Offer timeout (**slow: ~35 s**; `OFFER_TIMEOUT=5s make up` speeds it up) | No answer = rejection → next driver; a late accept → `409` | 5 |
| AC-06 · Ride state machine | Invalid transition → `409`; repeating a valid one is a no-op | 5 |
| AC-08 · Idempotency | Same key → original ride; key reused with another body → `422`; duplicate accept → `200`, replayed, valid state | 8 |
| AC-09 · Two rides, one driver | Exactly one ride gets the driver; after the winner cancels, the driver goes to the waiting ride | 6 |

AC-10 (event-driven dispatch) is exercised by every folder: each result is reached asynchronously through Redpanda. The gRPC API v2 is not in the collection (Postman's collection format has no gRPC requests); see [gRPC API v2 with grpcurl](#grpc-api-v2-with-grpcurl).

How to use it:

* **Run all:** right-click the collection → **Run** and keep the order. Or run a single folder the same way.
* **Run one request at a time:** open a folder and send its requests top to bottom. The first request of each folder sets up the scenario in its pre-request script.
* Each folder picks a **random spot on the map** and creates **its own drivers** through the driver-service fixture API (`{{driverServiceUrl}}/internal/drivers/…`), so folders never interfere with each other or with the seeded drivers.
* Dispatch is asynchronous, so requests that check a result first **wait in their pre-request script** (up to 10 s, or 45 s for the timeout) until the expected state is reached.
* Collection variables: `baseUrl` = `http://localhost:8080` (gateway), `driverServiceUrl` = `http://localhost:8082`. Change them under the collection's **Variables** tab if you remapped ports.

From the terminal (no Postman app needed): `make postman` runs the same collection with newman (needs Node). `make postman-gen` regenerates the file from [scripts/gen-postman.py](../scripts/gen-postman.py).

### By hand with curl

```bash
# 1. Request a ride (Idempotency-Key is required)
curl -s -X POST localhost:8080/rides \
  -H 'Idempotency-Key: my-first-ride' -H 'Content-Type: application/json' \
  -d '{"passengerId":"P001",
       "pickup":{"latitude":10.7769,"longitude":106.7009},
       "destination":{"latitude":10.8231,"longitude":106.6297}}' | jq
RIDE=<rideId from the response>

# 2. A moment later the nearest available seeded driver (D001, 222 m) holds an offer
curl -s localhost:8080/rides/$RIDE | jq '{status, assignedDriverId}'   # DRIVER_ASSIGNED, D001
curl -s localhost:8080/drivers/D001/offer | jq                           # the pending offer

# 3. Driver answers
curl -s -X POST localhost:8080/rides/$RIDE/offers/D001/reject | jq      # → D002 is offered next
curl -s -X POST localhost:8080/rides/$RIDE/offers/D002/accept | jq

# 4. Finish
curl -s -X POST localhost:8080/rides/$RIDE/complete | jq '.status'

# 5. What happened?
make trace-ride RIDE=$RIDE
curl -s localhost:8080/rides/$RIDE/history  | jq    # every status change
curl -s localhost:8080/rides/$RIDE/dispatch | jq    # saga, offers, exclusions, steps

# Drivers
curl -s -X PUT localhost:8080/drivers/D011/availability -d '{"status":"AVAILABLE"}' | jq
curl -s -X PUT localhost:8080/drivers/D011/location -d '{"latitude":10.7770,"longitude":106.7010}' | jq
```

Full API: [architecture.md §6](architecture.md#6-public-api).

### gRPC API v2 with grpcurl

Two driver-app APIs also exist as gRPC streams on the gateway's port **9090** ([ADR-010 Part 6](adr/ADR-010-api-and-identifiers.md#part-6-grpc-api-v2-for-two-driver-app-apis)).

```bash
# Terminal 1: driver D001 waits for offers (pushed, no polling). Ctrl+C to stop.
make grpc-offers DRIVER=D001

# Terminal 2: request a ride near D001 → terminal 1 prints an OFFERED event;
# accept or reject it with REST → terminal 1 prints CLOSED with the final status.
curl -s -X POST localhost:8080/rides -H 'Idempotency-Key: grpc-1' \
  -d '{"passengerId":"P001","pickup":{"latitude":10.7769,"longitude":106.7009},"destination":{"latitude":10.8231,"longitude":106.6297}}'

# Stream location updates for D010 (each one is acknowledged with the new version)
make grpc-location DRIVER=D010
```

Without make: `grpcurl -plaintext -import-path api/proto -proto ridehail/dispatch/v2/offers.proto -d '{"driverId":"D001"}' localhost:9090 ridehail.dispatch.v2.OfferService/WatchOffers`. Postman can also call gRPC: create a gRPC request, enter `localhost:9090`, and import the `.proto` file from [api/proto](../api/proto/).

### Look inside

| Want to see… | Where |
|---|---|
| Every log line of one ride, with HTTP bodies and message values | `make trace-logs TRACE=<traceId>` (`traceId` is in every ride response) |
| Logs, metrics, traces | Grafana http://localhost:3000 → Dashboards → **Dispatch overview** (type a `ride_id`, `driver_id` or `trace_id` in the boxes) |
| One ride's trace across services | Grafana → Explore → Tempo, or click a `trace_id` in a log line |
| Messages on each topic, consumer lag, DLQs | Redpanda Console http://localhost:8088 |
| Database rows | `make psql DB=dispatch_db`, then e.g. `select * from saga_steps order by id desc limit 20;` |
| The GEO index | `docker compose exec redis redis-cli GEOSEARCH geo:drivers:available FROMLONLAT 106.7009 10.7769 BYRADIUS 2 km ASC WITHDIST` |

## 5. Watch with lazydocker

```bash
lazydocker      # run from coding-challenge-3/kvp so it picks up the compose project
```

| Key | Action |
|---|---|
| ↑ ↓ | Pick a container (left panel) |
| `[` `]` | Switch tabs on the right: Logs, Stats, Env, Config, Top |
| `/` | Filter the log view, e.g. paste a ride id |
| `r` / `s` / `d` | Restart / stop / remove the container |
| `E` | Shell into the container (infrastructure images only; the Go images are distroless) |
| `b` | Bulk commands (stop all, …) |
| `q` | Quit |

**Failure drill:** request a ride, then stop `dispatch-service` with `s` before accepting. The ride stays where it is, and messages wait in Redpanda. Start it again with `r`: the saga resumes and a pending offer that expired meanwhile is rematched. The same drill from the shell:

```bash
docker compose kill dispatch-service
curl -s -X POST localhost:8080/rides -H 'Idempotency-Key: drill-1' -d '{"passengerId":"P002","pickup":{"latitude":10.7769,"longitude":106.7009},"destination":{"latitude":10.8,"longitude":106.7}}' | jq .status   # REQUESTED
docker compose up -d --wait dispatch-service    # a few seconds later the ride is DRIVER_ASSIGNED
```

## 6. Run the tests

| Command | What | Needs |
|---|---|---|
| `make test` | Unit tests: state machines, matching, validation | Go only |
| `make test-int` | Unit **+ integration** tests: real Postgres/Redis in throwaway containers (testcontainers-go): concurrency, idempotency, saga flows, GEO index | Colima running |
| `make test-e2e` | Black-box tests against the running stack through the gateway, including the 30 s offer timeout (~40 s) | `make up` first |
| `make test-all` | `test-int` + `test-e2e` | both |
| `make vet` | `go vet` for all build tags | Go |

Integration tests use the Docker engine in Colima. The Makefile sets `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`, the socket path inside the Colima VM, which the Ryuk cleanup container needs. If you run `go test -tags integration` directly, export it yourself. Skip the 30 s timeout test with `go test -tags e2e -short ./test/e2e/`.

Which test proves which requirement: [requirements-traceability.md](requirements-traceability.md); test strategy: [testing.md](testing.md).

## 7. Configuration

Set in [docker-compose.yml](../docker-compose.yml). The dispatch values can be overridden from the shell, e.g. `OFFER_TIMEOUT=5s make up`.

| Variable | Default | Service | Meaning |
|---|---|---|---|
| `MATCH_RADIUS_M` | `2000` | dispatch | Max pickup distance in metres |
| `OFFER_TIMEOUT` | `30s` | dispatch | Time a driver has to answer |
| `DISPATCH_RETRY_INTERVAL` | `10s` | dispatch | Retry when no driver was available (also retried at once when any driver becomes available) |
| `SWEEPER_INTERVAL` | `1s` | dispatch | How often expired offers are checked |
| `SAGA_STUCK_AFTER` | `30s` | dispatch | Re-send the last command if a saga got no reply for this long |
| `OUTBOX_POLL_INTERVAL` | `50ms` | all | Outbox relay polling |
| `CONSUMER_MAX_ATTEMPTS` | `5` | all | Handler retries before a message goes to the DLQ |
| `TOPIC_PARTITIONS` | `6` | all | Partitions when topics are first created |
| `LOG_LEVEL` | `info` | all | `debug` also logs outbox batches |
| `LOG_FORMAT` | `text` | all | `text` = plain `key=value` lines; `json` = one JSON object per line |
| `GRPC_ADDR` | `:9090` | driver, dispatch | gRPC listen address (API v2) |
| `OFFER_STREAM_POLL_INTERVAL` | `500ms` | dispatch | How often an open `WatchOffers` stream checks for a new or closed offer |
| `OFFER_STREAM_HEARTBEAT` | `15s` | dispatch | Keep-alive event on idle `WatchOffers` streams |

## 8. Troubleshooting

| Symptom | Fix |
|---|---|
| `docker: unknown command: docker compose` | Add `cliPluginsExtraDirs` (step 1) |
| `make up` fails with "port is already allocated" (8080, 9090, 3000, …) | Another program uses the port: `lsof -nP -iTCP:9090 -sTCP:LISTEN`, stop it or change the left side of the mapping in `docker-compose.yml` |
| `grpcurl: … connection refused` / `make grpc-offers` hangs | Stack not up (`make ps`), or call the gateway on `localhost:9090` with `-plaintext` |
| http://localhost:8080/docs/ shows "Failed to load API definition" | `docker compose restart gateway swagger-ui` (the gateway serves `api/openapi.yaml` from a file mount) |
| `Cannot connect to the Docker daemon` | `colima status` → `colima start`; check `docker context ls` / `$DOCKER_HOST` |
| Colima warns "DOCKER_HOST overrides the active context" | Harmless if `DOCKER_HOST` points at `~/.colima/docker.sock` |
| `make up` times out on `redpanda` or services restart in a loop | Not enough memory: `colima stop && colima start --cpu 4 --memory 8` |
| Port already in use (8080, 5432, 6379, 3000…) | `lsof -nP -iTCP:8080 -sTCP:LISTEN` and stop that process, or change the left side of `ports:` in the compose file |
| Integration tests: "Cannot connect to Docker" / Ryuk fails | Export `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`; as a last resort, `TESTCONTAINERS_RYUK_DISABLED=true` |
| Ride stuck in `MATCHING` | Expected when no `AVAILABLE` driver is within 2 km (or all were excluded). Check `GET /rides/{id}/dispatch` → `steps` |
| GEO index looks wrong after a Redis restart | `make rebuild-geo` |
| Start completely fresh | `make clean && make up` |
| Free your Mac's resources | `make down && colima stop` |
