# Egress Security Policy

The egress security (DPI) policy ships inside the **connect library** this
provider links (`github.com/full-bars/connect@v0.0.0-20260916141202`) — there
is no forked copy in this repo. Layered checks run in order on egress (and the
CFAA source check on ingress); the first layer that decides wins. Verdicts
(`ip_security.go`): **allow**, **drop**, or **incident** (drop + reported).

**0. SMTP policy (`ip_smtp_policy.go`)** — runs ahead of CFAA. Port 25 is
local-only; 465 must open with a TLS ClientHello; 587 allows only bounded
negotiation until STARTTLS → ClientHello. Per-flow state bounded at 1024 flows
(`smtpMaxFlowCount`); violations reset and count.

**1. CFAA endpoint reputation (`ip_security_cfaa.go`)** — packed, sorted,
non-overlapping blocklist of abused IP ranges, generated at build time from
threat feeds (pinned build: 46,789 IPv4 + 604 IPv6 prefixes). Blocked
destinations are refused on the egress hot path and on ingress.

**2. DMCA stateful inspection (`ip_security_dmca.go`)** — drops BitTorrent on
all ports: BEP 3 peer-wire/HTTP tracker, BEP 5 DHT (KRPC), BEP 15 UDP tracker,
BEP 29 uTP. Plaintext signature → incident; entropy heuristic on encrypted
payloads backstops obfuscated BitTorrent (MSE/PE over TCP, uTP over UDP).

**3. Web-standard matcher (`ip_security_webstandard.go`)** — clean-room RFC
recognizers for TLS (RFC 8446/5246), DTLS (6347/9147), QUIC (9000/9369), STUN
(5389) exempt legitimate encrypted traffic from the entropy heuristic.

The policy is compiled into the library and is **not tunable at runtime** — it
cannot be misconfigured away. Fork divergences are recorded in FORK_CHANGES.md.