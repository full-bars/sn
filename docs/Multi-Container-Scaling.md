# 🐳 Multi-Container Scaling

You can run multiple independent nodes in a single `docker-compose.yml` file. Every node is a separate container with its own state volume, its own auth code and its own client identity.

> [!WARNING]
> This image has no dashboard. It installs no vnStat, serves no port 8080 and has no vnStat volume, so the templates below publish no ports. Moving an existing `:latest` container onto this image drops the old dashboard; read [Docker Deployment](/full-bars/sn/wiki/Docker-Deployment) first.

Use the 3-node example if you want to understand the pattern. Use the 5-node or 10-node templates if you want a larger stack ready to copy.

## ⚙️ How It Works

Give each container its own config volume mounted at `/root/.urnetwork`. That directory holds the JWT, the client identities, the state database and the control socket `provider.sock`, and none of it can be shared between running providers. The control socket refuses to start when a live listener already holds the path, so a second provider on the same volume reports `status_failed` and exits within a second, and the state database is locked by whichever process opened it first.

1. Get one auth code per node. An auth code is single-use, so the nodes cannot share one.
2. Each node authenticates from its own `URNETWORK_AUTH_CODE` on its first start and keeps the resulting JWT in its own volume, so later restarts need no code.
3. Each node registers its own distinct client identity with the backend.

> [!NOTE]
> Node labels are **not implemented in this line**. Nothing reads `URNETWORK_NODE_NAME`, so the templates below do not set it and the dashboard shows no label. See [Node-Identity.md](/full-bars/sn/wiki/Node-Identity).

Once a node has started once you can delete its `URNETWORK_AUTH_CODE` line; the stored JWT is used first and the code is never read again.

## 📋 Choose a Template

| Stack Size | Best For |
| :--- | :--- |
| 3 nodes | Small deployments and learning the pattern. |
| 5 nodes | Moderate single-host scaling. |
| 10 nodes | Larger hosts where you want a ready fleet file. |

Each service needs a unique service name, container name and config volume. No host port is needed for ordinary provider traffic, because the miner dials out only. The image serves Prometheus `/metrics` by default. Publish a separate host port for each container only if you need host access to metrics. Replace each `AUTH_CODE_N` with that node's own auth code.


## 🚶 3-Node Walkthrough

```yaml
services:
  node-1:
    image: ghcr.io/full-bars/sn:latest
    container_name: urfix-1
    restart: unless-stopped
    pull_policy: always
    cap_add: [NET_ADMIN, NET_RAW]
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_1
    volumes:
      - ur_config_1:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

  node-2:
    image: ghcr.io/full-bars/sn:latest
    container_name: urfix-2
    restart: unless-stopped
    pull_policy: always
    cap_add: [NET_ADMIN, NET_RAW]
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_2
    volumes:
      - ur_config_2:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

  node-3:
    image: ghcr.io/full-bars/sn:latest
    container_name: urfix-3
    restart: unless-stopped
    pull_policy: always
    cap_add: [NET_ADMIN, NET_RAW]
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_3
    volumes:
      - ur_config_3:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  ur_config_1:
  ur_config_2:
  ur_config_3:
```

## 🚀 Ready Templates

For larger stacks, these templates use YAML anchors to keep the files shorter while still being ready to copy.

Replace each `AUTH_CODE_N` with a separate auth code for that node. A volume that already holds a JWT ignores the code.

### 🖐️ 5 Nodes

```yaml
x-urnetwork-common: &urnetwork-common
  image: ghcr.io/full-bars/sn:latest
  restart: unless-stopped
  pull_policy: always
  cap_add: [NET_ADMIN, NET_RAW]
  sysctls:
    - net.ipv4.ip_forward=1
  logging:
    driver: json-file
    options:
      max-size: "10m"
      max-file: "3"

services:
  node-1:
    <<: *urnetwork-common
    container_name: urfix-1
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_1
    volumes:
      - ur_config_1:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-2:
    <<: *urnetwork-common
    container_name: urfix-2
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_2
    volumes:
      - ur_config_2:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-3:
    <<: *urnetwork-common
    container_name: urfix-3
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_3
    volumes:
      - ur_config_3:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-4:
    <<: *urnetwork-common
    container_name: urfix-4
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_4
    volumes:
      - ur_config_4:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-5:
    <<: *urnetwork-common
    container_name: urfix-5
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_5
    volumes:
      - ur_config_5:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

volumes:
  ur_config_1:
  ur_config_2:
  ur_config_3:
  ur_config_4:
  ur_config_5:
```

### 🔟 10 Nodes

```yaml
x-urnetwork-common: &urnetwork-common
  image: ghcr.io/full-bars/sn:latest
  restart: unless-stopped
  pull_policy: always
  cap_add: [NET_ADMIN, NET_RAW]
  sysctls:
    - net.ipv4.ip_forward=1
  logging:
    driver: json-file
    options:
      max-size: "10m"
      max-file: "3"

services:
  node-1:
    <<: *urnetwork-common
    container_name: urfix-1
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_1
    volumes:
      - ur_config_1:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-2:
    <<: *urnetwork-common
    container_name: urfix-2
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_2
    volumes:
      - ur_config_2:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-3:
    <<: *urnetwork-common
    container_name: urfix-3
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_3
    volumes:
      - ur_config_3:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-4:
    <<: *urnetwork-common
    container_name: urfix-4
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_4
    volumes:
      - ur_config_4:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-5:
    <<: *urnetwork-common
    container_name: urfix-5
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_5
    volumes:
      - ur_config_5:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-6:
    <<: *urnetwork-common
    container_name: urfix-6
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_6
    volumes:
      - ur_config_6:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-7:
    <<: *urnetwork-common
    container_name: urfix-7
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_7
    volumes:
      - ur_config_7:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-8:
    <<: *urnetwork-common
    container_name: urfix-8
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_8
    volumes:
      - ur_config_8:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-9:
    <<: *urnetwork-common
    container_name: urfix-9
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_9
    volumes:
      - ur_config_9:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

  node-10:
    <<: *urnetwork-common
    container_name: urfix-10
    environment:
      - AUTH=authcode
      - URNETWORK_AUTH_CODE=AUTH_CODE_10
    volumes:
      - ur_config_10:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt

volumes:
  ur_config_1:
  ur_config_2:
  ur_config_3:
  ur_config_4:
  ur_config_5:
  ur_config_6:
  ur_config_7:
  ur_config_8:
  ur_config_9:
  ur_config_10:
```

## ▶️ Start the Stack

From the folder containing your chosen `docker-compose.yml`:

```bash
docker compose up -d
```

## ✅ Verify

Check a node's logs to confirm first-time authentication:

```bash
docker logs urfix-1
```

List running containers:

```bash
docker compose ps
```

Check your Client Manager. You should see one entry per node, identified by its client identity (not by `URNETWORK_NODE_NAME`, which this line does not read) and a redacted public IP for privacy, for example:

```text
urfix-1 @ 69.x.x.96 [v2026.x]
urfix-2 @ 69.x.x.96 [v2026.x]
```