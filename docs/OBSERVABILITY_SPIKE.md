# Observability spike: Quickwit + Jaeger + Prometheus + Grafana

Branch-local experiment. Nothing here runs by default; the base
`docker compose up` is untouched. Question under test: is Quickwit a
good dev-time home for kpr logs and traces?

## Run it

```bash
# Full stack from the repo (kpr + redis + registry + observability):
docker compose -f docker-compose.yml -f docker-compose.observability.yml up --build

# Or infra only, with kpr running locally from the repo:
docker compose -f docker-compose.yml -f docker-compose.observability.yml up quickwit jaeger prometheus grafana
OTEL_ENABLED=true OTEL_EXPORTER_OTLP_ENDPOINT=localhost:7281 go run ./cmd/app serve
```

Then generate traffic (`curl localhost:8080/api/hello`) and open:

| What | Where |
|---|---|
| Logs (searchable, survive `compose down`) | http://localhost:7280 (Quickwit UI) |
| Traces | http://localhost:16686 (Jaeger UI, storage = Quickwit) |
| Metrics targets | http://localhost:9090 (Prometheus) |
| Demo dashboard | http://localhost:3000, "kpr demo (spike)" (anonymous viewer) |
| Launchpad | http://localhost:9300 — console links all four when the overlay sets the `KPR_*_URL` vars |

Allow ~60s after traffic before expecting hits in Quickwit — indexing
is not instant (observed: <90s, tutorial says ~30s).

## What to feel for

1. Quickwit UI search over structured access logs: every request logs
   one line with `method/path/status/duration_ms` plus `trace_id` /
   `span_id` as filterable fields. Same `trace_id` in Jaeger finds the
   trace. stdout keeps one readable line per request — the IDs ride as
   trailing structured fields, not message text.
2. Jaeger waterfalls backed by Quickwit over gRPC (no separate
   Jaeger collector/storage).
3. Grafana dashboard: request rate by path, p50/p95 latency, and the
   `kpr_demo_queue_depth` gauge — synthetic sawtooth, clearly named,
   there only to prove the metrics path moves.

## Findings so far

- Quickwit 0.9.1 serves OTLP/gRPC (`otlp-traces`, `otlp-logs`) on
  `:7281` — that is the path kpr uses now. OTLP/HTTP exists only on
  non-standard `/api/v1/otlp/v1/*` paths on `:7280`; standard `/v1/*`
  404s. This forced the spike from OTLP/HTTP to OTLP/gRPC and moved
  the default endpoint `localhost:4318` → `localhost:4317`.
- Middleware nesting matters: `RequestTelemetry` must sit INSIDE
  `otelhttp` or the span is not in context yet and every log silently
  loses its IDs (hit this live; `TestTelemetryInsideMiddlewareSeesSpan`
  locks the order).
- The `otelslog` bridge (v0.20.1, latest) does NOT propagate span
  context into the OTLP envelope — but Quickwit promotes
  `trace_id`/`span_id` log attributes to top-level record fields, so
  correlation works anyway. Click-through trace↔log linkage beyond
  search-by-ID is unverified.
- Dependency side effect: the split `otlptracegrpc`/`otlploggrpc`
  modules pulled OTel core 1.38 → 1.46 and otelhttp 0.63 → 0.70 via
  MVS. Builds and full suite pass, but that bump needs a conscious
  decision if any of this is adopted.
- Metrics confirm Quickwit has no metrics story: Prometheus scrapes
  `/metrics/prometheus` on the management console. Exemplars are
  wired by the OTel exporter but not yet shown anywhere.

## If adopted, still open

- OTel collector between app and backends for cluster mode (app keeps
  one endpoint; compose stays direct). This is the "compose friendly,
  big clusters capable" hinge — the spike proves the direct mode.
- Grafana Quickwit datasource plugin for a single-pane option
  (deliberately skipped here: three native UIs first).
- Newer `otelslog` (or hand-rolled envelope IDs) for OTLP-level
  trace linkage if search-by-ID proves insufficient.
- No new env vars were needed: everything rides the existing `OTEL_*`
  set. `/metrics` (JSON) is untouched; Prometheus exposition lives at
  `/metrics/prometheus` and only exists when OTEL is enabled.
