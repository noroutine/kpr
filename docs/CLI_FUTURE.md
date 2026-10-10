# CLI — future work

Parked ideas, not built. Shipped layout lives in [CLI.md](CLI.md).

## Console parity: gateway and clock in `store status`

The console's keeper card shows two things the CLI `store
status` card does not: gateway posture (open/deny/held plus the
endpoints) and clock (method, server, live skew). The plan is
parity on the card, not new commands — status is the card, and
one command lives in its own file already.

- `gateway:` line voices the live fence posture through the
  same read the console renders (`fence.Gate` snapshot over the
  hold dir): open, deny, or held, with `EdgeAddr →
  RegistryURL` beside it. Disabled or unproven renders closed,
  never an error — same rule as the console section.
- `clock:` line voices `method server skew` through
  `clock.NewSource` + one bounded probe (the HTTPS transport
  already times out on its own; local never probes and reads
  "—"). Skew formats with `human` beside the exact offset;
  unreachable reads "unreachable", never slow.
- `--json` grows the same fields beside the text lines; probe
  failure keeps the card (degraded values, loud words), never
  fails it — status stays a pure read that cannot refuse.
- Tests stage both through env like production reads them:
  `TimeMethod=local` pins the no-probe line, an `httptest`
  date server behind `TimeServer` pins a skew line. No new
  seam: the factory move is what makes this stagable.
