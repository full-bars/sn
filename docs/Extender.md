# Let the network reach your provider on port 443

An extender is a provider that also listens on a port the internet can reach, so the network can publish your address and clients can connect through it. The network accepts each address family on its own, IPv4 and IPv6, and each one has its own `activate v4` or `activate v6` log line. This page walks you through publishing TCP 443 from a Docker provider, then reading the log to see whether the role started and whether the network accepted it. You need it only if the host has a public IPv4 address and you can open inbound TCP 443. A host behind NAT cannot take the role today.

## Before you start

- The extender role needs inbound TCP 443. That port needs privilege on Linux, FreeBSD, and macOS, and an inbound firewall rule on Windows.
- A normal install stays unprivileged at user level. Run `urnet-tools extender setup` as the one-time privileged configuration step to grant the required binding privilege or firewall access; this step can safely be re-run after an update or binary swap to repair dropped capabilities. Run `urnet-tools extender about` for a short description of the role and its ports.
- A host with a public IPv4 address where you control inbound TCP 443, both in the host firewall and in any router or cloud security group in front of it.
- A free TCP 443 on that host. If another process holds it, the role stays off while the provider keeps providing without listening.
- A provider that runs direct. A provider with proxies configured never listens, because the role must bind this host's own ports and publish this host's own address, and a proxied identity can do neither.
- A first-run auth code from [ur.io](https://ur.io), or a volume that already holds a signed-in provider.
- Docker containers are not automated by `extender setup`. For Docker, publish `-p 443:443 -p 443:443/udp -p 4053:4053/udp`, and supply either `--cap-add=NET_BIND_SERVICE` or `--network host`. Publishing is only half of it: the host firewall still has to allow the ports.

## Prepare the host with `urnet-tools extender setup`

```sh
urnet-tools extender setup
urnet-tools extender setup --dry-run
urnet-tools extender setup --yes
```

`urnet-tools extender setup` inspects the host and proposes only the missing privilege and firewall steps:

1. **Port 443 occupancy.** Checks whether another process already holds TCP 443 and stops with the holder's name and PID if one does.
2. **Low-port bind privilege.** On Linux, checks `getcap` on the installed provider binary (resolving symlinks) and the process capability bounding set (`/proc/self/status` `CapBnd`), and plans `setcap cap_net_bind_service=+ep <binary>` when missing. On FreeBSD, checks `mac_portacl` and `security.bsd.unprivileged_reserved_ports`.
3. **Host firewall rules.** Inspects the active local firewall (`ufw`, `firewalld`, `iptables`/`ip6tables`, `nftables`, `pf`/`ipfw`, or Windows Firewall) for inbound TCP 443 (required TLS carrier), UDP 443 (optional QUIC carrier) and UDP 4053 (optional DNS carrier), and plans only the rules that are not already present. For example, if `ufw` is active and already allows `443/tcp` while `443/udp` and `4053/udp` are missing, it proposes only `ufw allow 443/udp` and `ufw allow 4053/udp`.
4. **Confirmation gate.** Prints the port summary and every command it will run under `Commands to run:`, then asks `Apply these changes? [y/N]:` before changing anything. In a non-interactive session without `--yes` (or `--force`) it executes nothing and exits with an error; `--dry-run` (`-n`) prints the plan and exits without prompting.
5. **Bind verification.** On Linux, after capability checks, runs `<provider-binary> --extender-bind-probe` as the provider's user (`sudo -n -u <user>`) when TCP 443 is free to confirm a live low-port bind. When the running provider already holds TCP 443 or the probe cannot run without a password prompt, `setup` states plainly that the live TCP 443 bind was not tested.

Docker containers are not modified by `extender setup`; publish the ports on `docker run` or use `--network host` as shown below, and allow the ports in the host firewall.

## Steps

**1. Remove any proxy configuration from this provider.**

Only the direct slot can hold the role, so a container that is given proxies serves through them and does not listen. Start from a container with no `proxy.txt` mounted and no `PROXY_URL` set.

Expected result: the container starts and provides as before, with no proxy slots.

**2. Open TCP 443 on the host.**

Allow inbound TCP 443 in the host firewall, and forward it on the router or security group in front of the host. Optionally allow UDP 443, UDP 4053 and UDP 53 as well. The network uses them when offered, and none is required. To offer UDP 53 with Docker, also add `-p 53:53/udp` to the command in step 3.

Cloud providers block inbound ports by default. Add an inbound rule for these ports in the Oracle security list, the GCP VPC firewall rules, the AWS security group or the Azure network security group.

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
  -e ALLOW_CLIENT_REGISTRATION=Y \
  -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_config:/root/.urnetwork \
  ghcr.io/full-bars/sn:latest
```

The auth code is single-use and is only consumed on first run, when the volume is empty. The registration setting in this example authorizes a new identity. Keep the volume: it holds the provider identity as well as the sign-in. If an existing identity is missing, restore its volume instead of authorizing a replacement (see [Docker Deployment](https://github.com/full-bars/sn/wiki/Docker-Deployment)).

Expected result: `docker ps` shows the container running with `443/tcp` and `4053/udp` published.

**4. Find the listen lines in the log.**

```sh
docker logs urnetwork 2>&1 | grep '\[extender\]'
```

Expected result: one line per carrier that bound, for example (the log may prefix each with a timestamp):

```text
[extender] bound transport=tcp port=443 carrier=tcp
[extender] bound transport=udp port=443 carrier=quic
[extender] bound transport=udp port=4053 carrier=dns
```

The miner also tries UDP 53 for the DNS carrier. A `port=53` line appears when that port binds.

A carrier that failed to bind logs `[extender] bind failed transport=<tcp or udp> port=<port> carrier=<carrier> error=<reason>` instead. A failed UDP carrier leaves only that carrier down. If the TCP carrier fails, the role stays off while the provider keeps providing.

Then, from a machine on a different network, check that the port answers:

```sh
nc -vz YOUR_PUBLIC_IP 443
```

Expected result: `succeeded` or `open`. A timeout means the firewall or router rule from step 2 is not in place.

**5. Read the activation result.**

Run the same grep a few minutes later. The network dials your public address on the TCP carrier, checks the challenge signature, and performs a `GET /hello` through it. A refusal is logged with the network's own reason:

```text
[extender] activate v4 refused carriers=tcp api=... error=<the network's reason>
🌉⛔ [extender] IPv4 activation try 3 failed after 12.4s: <the network's reason>
[extender] activation refused; next attempt in 10m
```

A post that never got an answer is logged as `[extender] activate v4 failed carriers=tcp api=... error=...`. The error lists the cause of each attempt, for example a dial or DNS error.

When the network accepts an address family, the log shows it once:

```text
🌉✅ [extender] IPv4 is now activated on try 1. The platform can reach this host over IPv4.
```

Expected result: the latest IPv4 activation result is `is now activated`, or `urnet-tools extender status` shows IPv4 activated. An earlier `refused` line can remain in the log after a successful retry. If the latest result is a refusal, the reason names the test that failed; the usual cause is that the network could not reach TCP 443 on your public address, so recheck step 2 from outside your network. A failure that never reached the network reads differently: `[extender] activation failed without a refusal ...` costs no hold and usually means the path to the network is flaky, not that the port is closed. Every extender line and what it means is listed in the [log message reference](https://github.com/full-bars/sn/wiki/Log-Message-Reference).

**6. If the log says the launch was held, wait it out.**

```text
[extender] activation held: 5 of 5 posts in the last hour, next in 12m
```

The provider limits how often it asks the network to activate: five activation posts per hour per account, and after a refusal it waits 10 minutes, doubling on each further refusal up to one hour. During a hold the provider keeps providing; it just does not listen or activate.

Expected result: the role comes back on its own when the time shown has passed, without a restart.

## If the role does not come up after a restart

TCP 443 is the one port the role cannot do without. While it cannot bind, the role is fully off: no UDP carrier, no node, no activation.

After a restart another process may still hold that port for a moment. The miner retries quickly at first, seconds apart, and then every three minutes, so the role comes back on its own once the port is free. Restarting again before it settles only adds another wait: restart once and leave it.

## What this does not tell you

- `[extender] bound transport=tcp port=443` means the process bound the port inside the container. It does not mean the internet can reach it. Only the network's answer tells you that: the `is now activated` line. A refused probe is retried only after the hold in step 6.
- The `is now activated` line appears once, when the network accepts an address family. Renewals do not print it again, so a quiet log after that line is normal.
- An accepted activation does not mean clients will use you. The network publishes your address and clients choose among the addresses it lists.
- UDP 443, UDP 4053 and UDP 53 do not gate activation. A host with TCP 443 and no UDP is accepted; a host with both UDP ports and no TCP 443 is not.
- A host without inbound TCP 443 cannot run this role today. The network's design describes a peer-to-peer webrtc path for hosts behind NAT, but the provider has no switch for it and does not offer that carrier.
