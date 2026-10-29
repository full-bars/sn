# Let the network reach your provider on port 443

An extender is a provider that also listens on a port the internet can reach, so the network can publish your address and clients can connect through it. This page walks you through publishing TCP 443 from a Docker provider, then reading the log to see whether the role started and whether the network accepted it. You need it only if the host has a public IPv4 address and you can open inbound TCP 443. A host behind NAT cannot take the role today.

## Before you start

- A host with a public IPv4 address where you control inbound TCP 443, both in the host firewall and in any router or cloud security group in front of it.
- A free TCP 443 on that host. If another process holds it, the role stays off while the provider keeps providing without listening.
- A provider that runs direct. A provider with proxies configured never listens, because the role must bind this host's own ports and publish this host's own address, and a proxied identity can do neither.
- A first-run auth code from [ur.io](https://ur.io), or a volume that already holds a signed-in provider.

## Steps

**1. Remove any proxy configuration from this provider.**

Only the direct slot can hold the role, so a container that is given proxies serves through them and does not listen. Start from a container with no `proxy.txt` mounted and no `PROXY_URL` set.

Expected result: the container starts and provides as before, with no proxy slots.

**2. Open TCP 443 on the host.**

Allow inbound TCP 443 in the host firewall, and forward it on the router or security group in front of the host. Optionally allow UDP 443 and UDP 4053 as well; the network uses them when offered, and neither is required.

Expected result: the rules are saved. You test them from outside in step 4, once the provider is listening.

**3. Publish the carrier ports when you start the container.**

```sh
docker run -d \
  --name=urnetwork \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -p 443:443 \
  -p 443:443/udp \
  -p 4053:4053/udp \
  -e URNETWORK_AUTH_CODE=YOUR_AUTH_CODE_HERE \
  -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_config:/root/.urnetwork \
  ghcr.io/full-bars/sn:latest
```

The auth code is single-use and is only consumed on first run, when the volume is empty. Keep the volume: it holds the provider identity as well as the sign-in, and a container started on an empty volume registers a new provider (see [Docker Deployment](https://github.com/full-bars/sn/wiki/Docker-Deployment)).

Expected result: `docker ps` shows the container running with `443/tcp` and `4053/udp` published.

**4. Find the listen lines in the log.**

```sh
docker logs urnetwork 2>&1 | grep '\[extender\]'
```

Expected result: one line per carrier that bound, for example (the log may prefix each with a timestamp):

```text
[extender] listen tcp 443
[extender] listen udp 443 (quic)
[extender] listen udp 4053 (dns)
```

A carrier that failed to bind logs `[extender] listen error (<carrier>): <reason>` instead. A failed UDP carrier leaves only that carrier down. If the TCP carrier fails, the role stays off while the provider keeps providing.

Then, from a machine on a different network, check that the port answers:

```sh
nc -vz YOUR_PUBLIC_IP 443
```

Expected result: `succeeded` or `open`. A timeout means the firewall or router rule from step 2 is not in place.

**5. Read the activation result.**

Run the same grep a few minutes later. The network dials your public address on the TCP carrier, checks the challenge signature, and performs a `GET /hello` through it. A refusal is logged with the network's own reason:

```text
[extender]activate v4 refused = <the network's reason>
[extender] activation refused; next attempt in 10m
```

Expected result: no `refused` line. If you see one, the reason names the test that failed; the usual cause is that the network could not reach TCP 443 on your public address, so recheck step 2 from outside your network. A failure that never reached the network reads differently: `[extender] activation failed without a refusal ...` costs no hold and usually means the path to the network is flaky, not that the port is closed. Every extender line and what it means is listed in the [log message reference](https://github.com/full-bars/sn/wiki/Log-Message-Reference).

**6. If the log says the launch was held, wait it out.**

```text
[extender] activation held: 5 of 5 posts in the last hour, next in 12m
```

The provider limits how often it asks the network to activate: five activation posts per hour per account, and after a refusal it waits 10 minutes, doubling on each further refusal up to one hour. During a hold the provider keeps providing; it just does not listen or activate.

Expected result: the role comes back on its own when the time shown has passed, without a restart.

## What this does not tell you

- `[extender] listen tcp 443` means the process bound the port inside the container. It does not mean the internet can reach it. Only the absence of a refusal after the network's own probe tells you that, and a probe that fails is retried only after the hold in step 6.
- The provider's own log has no line that confirms an accepted activation, so a quiet log is the absence of a refusal, not a report of success.
- An accepted activation does not mean clients will use you. The network publishes your address and clients choose among the addresses it lists.
- UDP 443 and UDP 4053 do not gate activation. A host with TCP 443 and no UDP is accepted; a host with both UDP ports and no TCP 443 is not.
- A host without inbound TCP 443 cannot run this role today. The network's design describes a peer-to-peer webrtc path for hosts behind NAT, but the provider has no switch for it and does not offer that carrier.
