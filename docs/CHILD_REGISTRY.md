# Child registry (future effort)

Status: not started. Everything about a kpr-launched registry
lives here, out of [`docs/GATEWAY.md`](GATEWAY.md) — the gateway stays
external (`--registry=external`: kpr proxies a separately-run
registry) until this effort lands.

## Supervisor

`registry serve` as kpr's child process. Kills the
control-channel question: stop/collect/flush/start become one
ceremony with rollback, no socket. Until then the proxy, the
fence, and control events work against a registry kpr doesn't
launch.

## Control events

Registry control speaks the gc event mechanism, nothing new:
lifecycle `Event`s on the existing `Reporter` port
(`internal/gc/collector.go` — `Timed` stages, nil-safe `Emit`,
events shaped for a JSON-lines transport should the console
ever subscribe). Control stages join the gc ones as the
supervisor lands — of the set only spawn exists today
(`collector.go`); stop/restart, fence-hold/fence-release, and
ceremony phases are design, not code. An `Outcome` per
completed action goes into the activity ring
(`FileStore.PushActivity`, what `store status` shows). A
restart is as visible as a collect, through the same keys.
