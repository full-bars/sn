# Run a provider as an extender

An extender is a provider that also accepts traffic on a port the internet can reach, and the network publishes its address so clients can use it. The role is useful on a host with a public address and a free TCP 443; on a host behind NAT it cannot activate at all. This page covers the requirements, the Docker run that exposes the ports, and how to confirm the role came up.

## Before you start

- A host with a public IPv4 address, where you can open inbound TCP 443.
- A provider that runs **direct**. A provider with proxies configured never listens: the role needs to bind this host's own carrier ports and publish this host's own address, and a proxied identity can do neither, so the code enables the role only when no proxy settings are present.
- Permission from the network to provide as an extender. That gate is decided by the platform, not by a local setting, and a host that fails it is refused however its ports are configured.

## What has to be reachable

**TCP 443 is required.** The network verifies an extender by dialling its public address on the tcp carrier, checking the challenge signature, and performing a verified `GET /hello` through it. If that probe cannot reach you, activation is refused and the host is held for ten minutes before it tries again.

The UDP carriers are optional. They improve how clients reach you and none of them gates activation:

- UDP 443 carries QUIC with the `h3` ALPN.
- UDP 4053 is the DNS carrier's port.
- UDP 53 is a second DNS port the miner tries, and it usually collides with a resolver already on the host.

## Docker

The default run command in the install docs publishes no ports and is correct for a provider that only serves through proxies. To run this one as an extender, publish the carrier ports:

```sh
docker run -d \
  --name=urnetwork-provider \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -p 443:443 \
  -p 443:443/udp \
  -p 4053:4053/udp \
  -e URNETWORK_AUTH_CODE='YOUR_AUTH_CODE_HERE' \
  -e HOST_HOSTNAME=$(hostname) \
  -v urnetwork_data:/root/.urnetwork \
  ghcr.io/full-bars/sn:latest
```

Publishing is only half of it: the port has to be open on the host's firewall and forwarded to the host by whatever sits in front of it. On a home router that means a port forward for 443; a host behind carrier-grade NAT cannot do this at all. On a host with a public address, publishing the port is enough.

## Confirm the role came up

The startup log names each carrier it bound, then the activation result:

```text
[extender] listen tcp 443
[extender] listen udp 443 (quic)
[extender] listen udp 4053 (dns)
```

A successful activation is reported by the diagnostics event with the address activated. A refusal is logged with the network's own reason, for example:

```text
[extender]activate v4 refused = the tcp carrier did not answer: remote error: tls: internal error
[extender] activation refused; next attempt in 10m
```

That message means the network could not establish a connection to your carrier address. It is not a TLS problem on your side: check that the port is published, open on the firewall, and forwarded to this host.

## What this does not tell you

- `listen tcp 443` in the log means the container bound the port inside itself. It does not mean the internet can reach it, and the two are the difference between a provider that activates and one that is refused.
- An activation does not guarantee clients. The network publishes your address and clients choose from it.
- A provider with proxies configured will not listen, whatever ports you publish. Remove the proxy configuration first if you want the role.
- Peer-to-peer webrtc, which would let a NAT'd host extend without a public port, is described by the network's design but is not implemented in the provider yet. A host without inbound TCP 443 cannot run this role today.
