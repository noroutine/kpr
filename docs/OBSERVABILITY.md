# Observability: Quickwit + Jaeger + Prometheus + Grafana

Optional dev-time observability for kpr. Nothing here runs by default;
the base `docker compose up` is untouched. Quickwit stores logs and
traces (single OTLP/gRPC backend), Jaeger UI reads traces back from it,
Prometheus scrapes metrics, Grafana shows the dashboard.

## Run it

```bash
# Full stack from the repo (kpr + redis + registry + observability):
just up-observability   # or: make up-observability

# Base stack only:
just up                 # or: make up

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
| Dashboard | http://localhost:3000, "kpr observability" (anonymous viewer) |
| Launchpad | http://localhost:9300 — console links all four when the overlay sets the `KPR_*_URL` vars |

Allow ~60s after traffic before expecting hits in Quickwit — indexing
is not instant (observed: <90s, tutorial says ~30s).

## What you get

1. Quickwit UI search over structured access logs: every request logs
   one line with `method/path/status/duration_ms` plus `trace_id` /
   `span_id` as filterable fields. Same `trace_id` in Jaeger finds the
   trace. stdout keeps one readable line per request — the IDs ride as
   trailing structured fields, not message text.
2. Jaeger waterfalls backed by Quickwit over gRPC (no separate
   Jaeger collector/storage).
3. Grafana dashboard: request rate by path, p50/p95 latency, and the
   `kpr_example_queue_depth` gauge — synthetic sawtooth, clearly named,
   there only to prove the metrics path moves.
4. Sweeper activity in the same log index: every pass emits one
   `sweep pass` summary (`trigger/performed/planned/failed/untracked/
   skipped/dry_run`) plus one `sweep row` per resolved row
   (`repo/tag/reason/outcome`, `err` on failures) — the redis
   activity ring mirrored as searchable records, never a dump.
   Skipped ticks log too, so "nothing due" reads distinctly from
   "sweeper went quiet". Quickwit UI (or REST) query examples:

   ```
   body.message:sweep AND attributes.outcome:deleted
   body.message:sweep AND attributes.skipped:true
   ```

## Notes

- Quickwit 0.9.1 serves OTLP/gRPC (`otlp-traces`, `otlp-logs`) on
  `:7281` — that is the path kpr uses. OTLP/HTTP exists only on
  non-standard `/api/v1/otlp/v1/*` paths on `:7280`; standard `/v1/*`
  404s. Hence OTLP/gRPC throughout and the default endpoint
  `localhost:4317`.
- Middleware nesting matters: `RequestTelemetry` must sit INSIDE
  `otelhttp` or the span is not in context yet and every log silently
  loses its IDs (hit this live; `TestTelemetryInsideMiddlewareSeesSpan`
  locks the order).
- The `otelslog` bridge (v0.20.1, latest) does NOT propagate span
  context into the OTLP envelope — but Quickwit promotes
  `trace_id`/`span_id` log attributes to top-level record fields, so
  correlation works anyway. Click-through trace↔log linkage beyond
  search-by-ID is unverified.
- Dependency note: the split `otlptracegrpc`/`otlploggrpc`
  modules pulled OTel core 1.38 → 1.46 and otelhttp 0.63 → 0.70 via
  MVS. Builds and full suite pass.
- Metrics confirm Quickwit has no metrics story: Prometheus scrapes
  `/metrics/prometheus` on the management console plus the registry's
  debug `/metrics` (registry `http.debug` block, `registry:5001` job).
  Exemplars are wired by the OTel exporter but not yet shown anywhere.

## Follow-ups

- OTel collector between app and backends for cluster mode (app keeps
  one endpoint; compose stays direct). This is the "compose friendly,
  big clusters capable" hinge.
- Grafana Quickwit datasource plugin for a single-pane option
  (three native UIs for now).
- Newer `otelslog` (or hand-rolled envelope IDs) for OTLP-level
  trace linkage if search-by-ID proves insufficient.
- Telemetry rides the existing `OTEL_*` set plus four `KPR_*_URL`
  console-link vars. `/metrics` (JSON) is untouched; Prometheus
  exposition lives at `/metrics/prometheus` and only exists when OTEL
  is enabled.
