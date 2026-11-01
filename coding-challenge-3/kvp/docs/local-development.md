# Local Development (Colima + Docker Compose + lazydocker)

No Docker Desktop. **Colima** runs a small Linux VM with the Docker engine, and the normal `docker` CLI talks to it.

## 1. One-time setup (macOS, Homebrew)

```bash
brew install colima docker docker-compose docker-buildx lazydocker go
```

Make the Docker CLI find the Compose/Buildx plugins. Add this to `~/.docker/config.json`:

```json
"cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]
```

Start the VM. Apple Silicon: `vz` + Rosetta is the fastest option. The stack needs about 4 CPU / 8 GB:

```bash
colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --vz-rosetta
docker context use colima          # or: export DOCKER_HOST=unix://$HOME/.colima/docker.sock
docker compose version && docker info --format '{{.ServerVersion}}'
```

Optional: start Colima at login with `brew services start colima`.

## 2. Run everything

```bash
cd coding-challenge-3/kvp
make up          # docker compose up -d --build  (migrations + seed run as one-shot jobs)
make ps          # status
make logs        # follow all service logs
make down        # stop (keeps volumes);  make clean  → also removes volumes
```

| Container | Purpose | Host port |
|---|---|---|
| `gateway` (nginx) | Public API | **8080** |
| `ride-service` / `driver-service` / `dispatch-service` | Go services | 8081 / 8082 / 8083 |
| `postgres` | `ride_db`, `driver_db`, `dispatch_db` | 5432 |
| `redis` | GEO index | 6379 |
| `redpanda` | Kafka API | 19092 |
| `redpanda-console` | Topic browser | **8088** |
| `lgtm` (grafana/otel-lgtm) | Grafana / OTLP | **3000** / 4317, 4318 |
| `migrate`, `seed` | One-shot jobs | — |

## 3. Inspect with lazydocker

```bash
lazydocker
```

* Left panel: pick a container. `Logs` tab streams its logs; `/` filters (type a ride id).
* `r` restart · `s` stop · `d` remove · `e` exec a shell · `m` view stats · `[ ]` switch tabs.
* Use it to kill `dispatch-service` mid-ride and watch the saga resume (failure demo).

## 4. Try it

```bash
# make two drivers available near District 1
curl -X PUT localhost:8080/drivers/D001/availability -d '{"status":"AVAILABLE"}'
curl -X PUT localhost:8080/drivers/D001/location -d '{"latitude":10.7772,"longitude":106.7010}'

# request a ride
curl -i -X POST localhost:8080/rides -H 'Idempotency-Key: demo-1' -d '{
  "passengerId":"P001",
  "pickup":{"latitude":10.7769,"longitude":106.7009},
  "destination":{"latitude":10.8231,"longitude":106.6297}}'

curl localhost:8080/drivers/D001/offer
curl -X POST localhost:8080/rides/<rideId>/offers/D001/accept
make trace-ride RIDE=<rideId>
```

`scripts/demo.sh` runs the full scenario (reject → rematch → accept → complete, duplicate request, concurrent race).

## 5. Tests

```bash
make test          # unit tests (no Docker)
make test-int      # integration: testcontainers-go starts Postgres/Redis/Redpanda in Colima
make test-e2e      # against the running compose stack
```

testcontainers with Colima needs:

```bash
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
export TESTCONTAINERS_RYUK_DISABLED=true   # only if Ryuk fails to start
```

(The Makefile sets both.)

## Troubleshooting

| Symptom | Fix |
|---|---|
| `docker: unknown command: docker compose` | Add `cliPluginsExtraDirs` (step 1) |
| `Cannot connect to the Docker daemon` | `colima status` → `colima start` |
| Services OOM / Redpanda restarts | `colima stop && colima start --memory 8` |
| Ports in use | `lsof -i :8080` and stop the other process |
