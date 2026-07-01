# Monitoring (Grafana + ClickHouse)

Grafana runs alongside ClickHouse in `compose.yml` and connects through the
official **ClickHouse data source** plugin (`grafana-clickhouse-datasource`).
There is a single data source: ClickHouse. Server stats come from the built-in
`system.*` tables; the indexer's own runtime stats are written into a
`monitoring.indexer_metrics` table by the indexer itself.

## Quick start

```sh
docker compose up -d            # starts clickhouse + grafana
open http://localhost:3001      # login: admin / admin (override GRAFANA_PORT/creds in .env)
```

Two dashboards are provisioned automatically under the **sqd-go** folder:

| Dashboard            | Source                          | Shows |
|----------------------|---------------------------------|-------|
| **ClickHouse Server**| `system.metric_log`, `system.asynchronous_metric_log`, `system.asynchronous_metrics` | RSS/jemalloc memory, CPU cores, query/insert/select/merge throughput, bytes in/out, active parts, concurrency |
| **Indexer Runtime**  | `monitoring.indexer_metrics`    | blocks/s, events/s, heap alloc/sys, allocation rate, GC CPU % and pause, goroutines, CPU cores, checkpoint lag |

The data source, plugin, and dashboards are all provisioned from disk — no
manual setup in the Grafana UI is required.

## Enabling the indexer runtime metrics

The **ClickHouse Server** dashboard works immediately. The **Indexer Runtime**
dashboard is empty until the indexer writes to `monitoring.indexer_metrics`,
which is **opt-in** so it never affects benchmark runs:

```sh
SQD_METRICS_CH=1 sqd-go start ...      # or `make` targets / your run command
```

While enabled, a background goroutine (its own ClickHouse connection, off the
ingestion hot path) samples `runtime.MemStats` + process CPU every 5s and writes
one row per chain. Tunables:

| Env var                   | Default | Meaning |
|---------------------------|---------|---------|
| `SQD_METRICS_CH`          | unset   | Set to any value to enable the writer |
| `SQD_METRICS_CH_INTERVAL` | `5s`    | Sampling/insert cadence (Go duration) |
| `SQD_METRICS_CH_TTL_DAYS` | `7`     | Retention of the metrics table |

The ingestion loop only calls `monitoring.Observe(...)` from its existing 5s
stats tick — a non-blocking in-memory snapshot update. All network I/O happens
on the writer goroutine.

## Read-only ClickHouse user

Per the ClickHouse Grafana docs, the data source connects as a **read-only**
user defined in [`clickhouse/users.d/grafana.xml`](../clickhouse/users.d/grafana.xml):
`readonly=1` with a `changeable_in_readonly` constraint on `max_execution_time`
(required by the clickhouse-go client). Credentials default to
`grafana` / `grafana_ro` (local only) and are wired through `.env`
(`GRAFANA_CH_USER`, `GRAFANA_CH_PASSWORD`) into the provisioned data source.

## Files

```
compose.yml                                   # grafana service + clickhouse users.d mount
clickhouse/users.d/grafana.xml                # read-only user
grafana/provisioning/datasources/clickhouse.yaml
grafana/provisioning/dashboards/dashboards.yaml
grafana/dashboards/clickhouse-server.json
grafana/dashboards/indexer-runtime.json
internal/monitoring/recorder.go               # indexer metrics writer (SQD_METRICS_CH)
```
