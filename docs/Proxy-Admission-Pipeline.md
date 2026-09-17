# Proxy Admission Pipeline

How the provider decides which proxies to trust before they enter the auth
queue. This is the quality-control layer. Every proxy is probed, verified, and
graded before it can carry real traffic.

## The three-stage probe

Before a candidate proxy is admitted, the provider runs a three-stage check
against it (`proxy_probe.go`):

1. **SOCKS5 greeting.** Is this actually a SOCKS5 proxy? The provider sends
   the protocol greeting and expects a valid version response. Anything else
   is rejected immediately.

2. **SOCKS5 CONNECT to the API.** Can the proxy reach the platform? The
   provider asks the proxy to open a CONNECT tunnel to `api.bringyour.com:443`.
   A proxy that cannot reach the API is useless and is rejected.

3. **TLS handshake through the proxy.** This is the anti-MITM stage. After
   the CONNECT succeeds, the provider runs a real TLS handshake through the
   proxy against the API host. The TLS config pins the server name and uses
   the system root store. A proxy that relays the handshake transparently
   passes. A proxy that terminates TLS itself, an intercepting or MITM proxy,
   presents its own certificate. The handshake fails verification. The proxy
   is classified as `probeTLSFailed` and is **never admitted** to the pool. It
   is routed to the reaper's failure and blacklist lifecycle instead.

A MITM proxy would otherwise decrypt and inspect every byte of client traffic
it relays. This stage makes that impossible to hide.

## The probe target table

The reachability probe does not just test the API. It samples a table of
commonly visited endpoints (`probe_targets.go`). This verifies each proxy can
actually reach the open internet.

The table holds **150 targets**:

- **127 health hostnames**, dialed on port 443. These are ordinary, widely
  reachable sites such as search engines, connectivity checkers, and CDN
  endpoints.
- **23 DNS resolver IPs**, queried on port 53.

Each probe pass samples a deterministic, disjoint block of the table.
Consecutive passes walk the whole table, so every proxy is eventually tested
against every endpoint. Scoring is positive-evidence-only: a successful TCP
handshake proves reachability; silence never convicts. DNS resolution happens
outside the probed channel so a poisoned resolver cannot fake a pass.

### Why the reputation sites are absent

The original target list also had a reputation class: Akamai, Reddit, Epic,
Stack Overflow, Reuters, Etsy, Ecosia, Canva. Those sites are deliberately
excluded at compile time. There is no runtime setting that turns them back on.

The reason is operational safety. Probing those reputation-sensitive sites is
the traffic pattern that gets an egress IP listed. Their abuse teams flag
exactly this behavior. A proxy provider whose boxes dial Reddit and Akamai on
a probe schedule will end up blocklisted. The table ships with the health and
DNS classes only. The provider can prove general reachability without tripping
reputation defenses.

## Probing before the auth queue

Admitted candidates still do not all authenticate at once. The provider runs
a weighted admission lottery for auth slots (`proxy_admission_gate.go`).

Each waiting proxy holds ticket weight `1 / (failures + 1)`. An untried proxy
is heavily favored. A proxy that has failed many times is never fully
excluded, but its chance is much smaller. Selection is random, weighted by
ticket value, so a long-shot retry still gets picked occasionally instead of
being starved out by a steady stream of high-weight untried proxies. This
prevents dead proxies from starving the queue while still letting them retry.

A concurrency semaphore caps in-flight auth attempts at **5**, independent of
the rate limiter. The rate limiter controls how fast attempts are released.
The semaphore caps how many are in flight at once. A slow, hanging proxy
cannot pile up unlimited parallel connections to the API.

## The admission outcome

A proxy that passes the probe stages and wins an auth slot is then graded. The
stage-1 table probe assigns an A-F grade (see the
[Stage-1 Quality Gate](Proxy-URL-Sources.md) section in Proxy URL Sources).
Proxies that fail any probe stage or TLS verification are never admitted. They
enter the failure and blacklist lifecycle instead.

The result is a pool of trusted proxies. Each one demonstrably works. Each one
reaches the open internet. Each one relays traffic without intercepting it.