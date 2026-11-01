# Docs — Ride-Hailing Dispatch Service

| Doc | What's inside |
|---|---|
| [requirements.md](requirements.md) | ACs + team constraints mapped to owners and mechanisms, scope, assumptions |
| [architecture.md](architecture.md) | System diagram, services, API, state machines, saga, topics, data model, trade-offs, failure modes, idempotency & concurrency summaries |
| [sequence-diagram.md](sequence-diagram.md) | Happy path, reject/rematch, timeout, concurrency race, duplicate delivery, cancel races |
| [scalability.md](scalability.md) | 1k → 100k → 1M drivers: estimates, bottlenecks, strategies, trade-offs |
| [bottlenecks-strategies-tradeoffs.md](bottlenecks-strategies-tradeoffs.md) | The required 3 bottlenecks / 3 strategies / 2 trade-offs, numbered |
| [observability.md](observability.md) | Log schema, "what happened to ride X?" runbook, dashboards, alerts |
| [local-development.md](local-development.md) | Colima + Compose + lazydocker setup, ports, demo, tests |
| [implementation-plan.md](implementation-plan.md) | Repo layout, libraries, test-to-file map, build order, config |
| [adr/](adr/) | 001 matching · 002 concurrency · 003 saga orchestration · 004 messaging (outbox/inbox) · 005 observability |
