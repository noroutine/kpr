# TLS: one switch, filed bundles <!-- omit in toc -->

One env switch serves HTTPS: `KPR_TLS_BUNDLE` names the
bundle every listener — console, app, receiver, and edge —
serves. The bundle lives in the config dir, so identity is
filed once and served everywhere: no per-listener cert paths.
Empty (the default) serves plaintext. A name with no bundle,
a parked bundle, or a bundle with no identity refuses the
boot — a listener that names a bundle with no identity
serves nothing. The boot log names the bundle and the floor
it serves (`tls: serving bundle "local" ...`), and each
server logs the scheme it bound (`http`/`https`), so a
scrolled boot log says which it is.

## Contents <!-- omit in toc -->

- [The switch](#the-switch)
- [Filed bundles (config dir)](#filed-bundles-config-dir)
- [Reload](#reload)
- [Standing model](#standing-model)
- [Local dev stack](#local-dev-stack)

## The switch

| Setting | Effect |
|---|---|
| `KPR_TLS_BUNDLE` empty (default) | plaintext on every listener |
| `KPR_TLS_BUNDLE=local` | HTTPS on every listener with the `local` bundle's cert and floor |

`KPR_MANAGEMENT_ENABLED=false` and `KPR_APP_ENABLED=false`
still switch servers off entirely; they answer to exactly
`"false"` — anything else keeps serving, so a typo never
takes a server down silently.

## Filed bundles (config dir)

`KPR_CONFIG_DIR` points at a folder of k8s-style YAML
files. Today the only kind is `TLSBundle` — see
[`config/bundles/example.yaml`](../config/bundles/example.yaml)
for the shape. Loading validates everything first: unknown
kinds and apiVersions, missing names, duplicate names, unknown
`minTLS` words, unknown fields (strict), unreadable cert/key
files, and bundles a listener would need with no identity.
One broken file rejects the whole load — serve keeps the old
snapshot (at boot: refuses), so a broken edit never
half-applies.

`minTLS` floors the negotiated version for every future
listener using the bundle: `"1.2"`, `"1.3"`, or empty for
Go's default. Older words refuse the load instead of
downgrading everything behind the bundle. `enabled: false`
parks a bundle: the load still validates and materializes it,
so re-enabling is a pure reload.

Trust-only bundles (a `caFile` with no leaf) are valid: they
materialize a pool and no cert, and card as `ok` — a CA file
with no leaf is configured, not missing. The loader checks
every filed CA file for chain gaps: an intermediate whose
issuer sits in neither its own file nor any other bundle's
cards `incomplete`, naming the missing issuer. The check is
presence-only over filed material — leaf issuance and the
system roots are out of scope — and it marks cards, never
the load.

## Traefik front (dev)

`just up-traefik` (or `make up-traefik`) layers a Traefik front
that terminates TLS with the machine's certbot identity
(`~/.certbot`, mounted read-only — `live/` entries are
symlinks into `../archive/`, so the whole tree mounts) and
routes the app by hostname, and the console by its own
hostname (`sneaky-fox-07.topteil.dev`, separate cert). The
backend stays plaintext; the console also stays directly
exposed on `:9300`. Both certbot hostnames are hardcoded in
`docker-compose.traefik.yml` and `dev/traefik/dynamic.yml` —
point both files at your own machine's identity before
running (plus a matching `/etc/hosts` line each).

## Local dev stack

`just up` stages the fixtures first (`dev-certs`) but boots
plaintext with no config dir — the base stack stays
unconfigured:

- `dev/config/bundles/` (committed): `local` (the staged
  identity, floor 1.3), `expired` (a real dead cert — expiry
  is a standing, not a load error), `trust` (the local cert
  as its own trust anchor), `mozilla` (the public roots —
  both trust-only, both read `ok`).
- `dev/tls/` (gitignored, generated): the PEMs the bundles
  name. Mozilla roots refresh every staging run, so the dev
  trust stays current; generated fixtures persist until
  deleted (delete one and rerun `just dev-certs` to
  reissue it).
- `just up-tls` layers the `docker-compose.tls.yml` overlay,
  which mounts both folders read-only at `/etc/kpr/` and sets
  one switch (`KPR_TLS_BUNDLE=local`), and every listener
  serves HTTPS with that bundle (browsers warn — expected for
  self-signed local dev).

Container paths, never host ones: the YAML names the mount
point, so the same folder loads on any checkout.

## Reload

`POST /api/config/reload` re-reads the dir and swaps the
snapshot whole: validation, then swap, never in between. Two
concurrent POSTs serialize, so the older snapshot cannot land
last. A failed reload keeps serving the old snapshot and parks
the error on the holder: the dashboard shows it, `status
bundles` keeps reading the old cards. The response names the
applied bundle count.

Serving listeners read their TLS at boot — a reload swaps
what the dashboard and `status` show, then a restart applies
it to the sockets. The endpoint says so in its response.

## Standing model

One vocabulary everywhere — the dashboard and `status
bundles` read the same cards (`internal/console.StandBundle`):

| Standing | Means |
|---|---|
| `ok` | leaf valid, thirty or more days left — or trust-only (a CA file with no leaf is configured, not missing) |
| `degraded` | leaf valid, under thirty days left |
| `expired` | past NotAfter |
| `parked` | disabled by `enabled: false` |
| `unproven` | identity files named but no leaf parsed — missing evidence never alarms |
| `incomplete` | CA material names an issuer no bundle carries — the chain has a hole |

Each card also voices the leaf (`subject · issued by issuer ·
expires date`, or the trust source for CA-only bundles), the
lifetime (`90 days left`, `expired yesterday`, `trust only`,
`unknown`), and the pinned floor. `minTLS` voices the pinned
floor, empty for Go's default — an unpinned bundle voices
nothing, so the default never prints as a promise. `status
bundles --json` carries every card field for scripts.
