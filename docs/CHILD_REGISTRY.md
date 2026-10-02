# Child registry (future effort)

Status: not started. Everything about a kpr-launched registry
lives here, out of `docs/GATEWAY.md` — the gateway stays
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
same JSON-lines transport), with control stages beside the gc
ones (spawn/stop/restart, fence-hold/fence-release, ceremony
phases), and an `Outcome` per completed action into the
activity ring (`FileStore.PushActivity`, what `store status`
shows). A restart is as visible as a collect, through the same
keys.
