# ADR-010: Local runtime, packaging and gateway: Colima + Docker Compose + nginx, distroless images

* **Status:** Accepted
* **Date:** 2026-11-01
* **Requirements:** team constraint T4 (run everything with Docker; lightweight engine + lazydocker via brew; no Docker Desktop); challenge §2 ("document how to start external infrastructure")
* **Related:** [local-development.md](../local-development.md)

## Context

Ten containers (3 services, Postgres, Redis, Redpanda, Redpanda Console, LGTM, gateway, plus test containers) must start with one command on a Mac without Docker Desktop. Clients need a single API entry point even though three services serve it.

## Part 1: container engine on macOS

| Option | License / cost | Resource use | Compose / BuildKit | testcontainers support | Setup | Verdict |
|---|---|---|---|---|---|---|
| Docker Desktop | Paid subscription for larger companies | Heavier VM + GUI | ✅ | ✅ | GUI installer | ❌ Excluded by T4 |
| **Colima** (Lima VM + Docker engine) | MIT, free | Light; `vz` + Rosetta on Apple Silicon | ✅ (CLI plugins via Homebrew) | ✅ (needs `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE`) | `brew install colima docker` | ✅ **Chosen** |
| OrbStack | Free for personal use, paid for commercial | Lightest, fastest | ✅ | ✅ | GUI app | ❌ Commercial license; T4 asks for brew + CLI |
| Rancher Desktop | Apache 2.0 | Medium (includes k3s) | ✅ (moby mode) | ✅ | GUI app | ❌ Heavier, Kubernetes not needed |
| Podman (machine) + podman-compose | Apache 2.0 | Light | ⚠️ compose compatibility gaps | ⚠️ needs extra configuration (Ryuk) | brew | ❌ More friction |
| Lima (raw) | Apache 2.0 | Light | Manual | Manual | More manual | ❌ Colima wraps it |

## Part 2: orchestration for local development

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Docker Compose** (health checks, `depends_on: service_healthy`, `--wait`) | One file; everyone knows it; health-gated start order | Not production orchestration | ✅ **Chosen** |
| Kubernetes locally (kind, k3d, minikube) + Helm | Production-like | Much heavier; Kubernetes is explicitly out of scope | ❌ |
| Tilt / Skaffold | Live reload on Kubernetes | Requires Kubernetes | ❌ |
| Run binaries directly + infrastructure in Docker | Fast inner loop | Several terminals; not "everything in Docker" | Possible for development (`go run`) |

**Inspection:** lazydocker (T4) is a terminal UI over the same Docker API: logs, stats, restart and exec per container. Alternatives are Portainer (web UI container, heavier), ctop (stats only) and k9s (Kubernetes only).

## Part 3: images

| Option | Size | Attack surface | Debuggability | Verdict |
|---|---|---|---|---|
| **Multi-stage build → `gcr.io/distroless/static-debian12:nonroot`** | ~15–25 MB | No shell, no package manager, non-root | No shell (health check is a subcommand of the binary: `/app healthcheck`) | ✅ **Chosen** |
| `scratch` | Smallest | Minimal | No CA certificates or `/etc/passwd` unless copied | ❌ Slightly more work for no gain |
| `alpine` | ~10 MB + binary | Shell + apk | Shell for debugging | ❌ Bigger surface |
| `golang` image as runtime | ~800 MB | Large | Full toolchain | ❌ |
| Buildpacks (`pack`), ko | Small, no Dockerfile | Minimal | — | ❌ Extra tooling; ko is a good alternative for Go |

One Dockerfile builds all three services (`--build-arg SERVICE=ride|driver|dispatch`) with BuildKit cache mounts for Go modules and the build cache.

## Part 4: API gateway

| Option | Routing by path | Footprint | Config | Extras | Verdict |
|---|---|---|---|---|---|
| **nginx 1.29** | ✅ regex locations | ~10 MB | One small file | JSON access log; Docker DNS re-resolution with `resolver` + variables | ✅ **Chosen** |
| Traefik v3 | ✅ via labels | ~50 MB | Labels on each service | Auto-discovery from Docker, dashboard | Strong alternative |
| Envoy | ✅ | ~50 MB | Verbose YAML | Advanced load balancing, gRPC, mTLS | ❌ Overkill |
| Kong / KrakenD / Tyk | ✅ | Medium–heavy | Plugins, DB (Kong) | Auth, rate limiting, developer portal | ❌ Auth etc. out of scope |
| Caddy | ✅ | ~40 MB | Caddyfile | Auto-TLS | Viable |
| No gateway (call services on their ports) | — | — | — | — | ❌ Leaks service boundaries to clients; `/rides/{id}/offers/…` spans two services |

The gateway does **not** expose `/internal/*` (fixtures, GEO rebuild). We learned the hard way that nginx resolves upstream names once at start-up. After a service container is recreated with a new IP, the gateway kept calling the old one, so upstreams are now variables resolved through Docker DNS (`127.0.0.11`).

## Decision

Colima + Docker Compose + lazydocker; distroless multi-stage images; nginx gateway on `:8080`; `make up` = `docker compose up -d --build --wait`.

## Consequences

* ✅ One command, ~30 s from clean to healthy, no Docker Desktop.
* ✅ Small, non-root images; health checks without a shell.
* ⚠️ Compose is not production: no autoscaling, no rolling deploys. Production would use Kubernetes (or a managed container service) with the same images.
* ⚠️ Homebrew's Compose/Buildx need `cliPluginsExtraDirs` in `~/.docker/config.json` (documented).
