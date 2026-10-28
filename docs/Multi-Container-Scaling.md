# 🐳 Multi-Container Scaling

You can run multiple independent nodes in a single `docker-compose.yml` file. Every node is a separate container with its own state volume, its own auth code and its own client identity.

Use the 3-node example if you want to understand the pattern. Use the 5-node or 10-node templates if you want a larger stack ready to copy.

## ⚙️ How It Works

Give each container its own config volume mounted at `/root/.urnetwork`. That directory holds the JWT, the client identities, the state database and the control socket `provider.sock`, and none of it can be shared between running providers. The control socket refuses to start when a live listener already holds the path, so a second provider on the same volume reports `status_failed` and exits within a second, and the state database is locked by whichever process opened it first.

1. Get one auth code per node. An auth code is single-use, so the nodes cannot share one.
2. Each node authenticates from its own `URNETWORK_AUTH_CODE` on its first start and keeps the resulting JWT in its own volume, so later restarts need no code.
3. Each node registers its own distinct client identity with the backend and reports to your dashboard with its own label.

Once a node has started once you can delete its `URNETWORK_AUTH_CODE` line; the stored JWT is used first and the code is never read again.

## 📋 Choose a Template

| Stack Size | Best For |
| :--- | :--- |
| 3 nodes | Small deployments and learning the pattern. |
| 5 nodes | Moderate single-host scaling. |
| 10 nodes | Larger hosts where you want a ready fleet file. |

Each service needs a unique service name, container name, host port, config volume and vnStat volume. Replace each `AUTH_CODE_N` with that node's own auth code.

## 🚶 3-Node Walkthrough

```yaml
services:
  # Each node has its own auth code, state volume and vnStat volume
  node-1:
    image: ghcr.io/full-bars/sn:latest
    container_name: urfix-1
    restart: unless-stopped
    pull_policy: always
    cap_add: [NET_ADMIN, NET_RAW]
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-1
      - URNETWORK_AUTH_CODE=AUTH_CODE_1
    volumes:
      - ur_config_1:/root/.urnetwork
      - urfix-1_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"
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
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-2
      - URNETWORK_AUTH_CODE=AUTH_CODE_2
    volumes:
      - ur_config_2:/root/.urnetwork
      - urfix-2_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9002:8080"
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
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-3
      - URNETWORK_AUTH_CODE=AUTH_CODE_3
    volumes:
      - ur_config_3:/root/.urnetwork
      - urfix-3_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9003:8080"
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  ur_config_1:
  ur_config_2:
  ur_config_3:
  urfix-1_vnstat: # Unique traffic stats per node
  urfix-2_vnstat:
  urfix-3_vnstat:
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
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-1
      - URNETWORK_AUTH_CODE=AUTH_CODE_1
    volumes:
      - ur_config_1:/root/.urnetwork
      - urfix-1_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"

  node-2:
    <<: *urnetwork-common
    container_name: urfix-2
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-2
      - URNETWORK_AUTH_CODE=AUTH_CODE_2
    volumes:
      - ur_config_2:/root/.urnetwork
      - urfix-2_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9002:8080"

  node-3:
    <<: *urnetwork-common
    container_name: urfix-3
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-3
      - URNETWORK_AUTH_CODE=AUTH_CODE_3
    volumes:
      - ur_config_3:/root/.urnetwork
      - urfix-3_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9003:8080"

  node-4:
    <<: *urnetwork-common
    container_name: urfix-4
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-4
      - URNETWORK_AUTH_CODE=AUTH_CODE_4
    volumes:
      - ur_config_4:/root/.urnetwork
      - urfix-4_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9004:8080"

  node-5:
    <<: *urnetwork-common
    container_name: urfix-5
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-5
      - URNETWORK_AUTH_CODE=AUTH_CODE_5
    volumes:
      - ur_config_5:/root/.urnetwork
      - urfix-5_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9005:8080"

volumes:
  ur_config_1:
  ur_config_2:
  ur_config_3:
  ur_config_4:
  ur_config_5:
  urfix-1_vnstat:
  urfix-2_vnstat:
  urfix-3_vnstat:
  urfix-4_vnstat:
  urfix-5_vnstat:
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
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-1
      - URNETWORK_AUTH_CODE=AUTH_CODE_1
    volumes:
      - ur_config_1:/root/.urnetwork
      - urfix-1_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9001:8080"

  node-2:
    <<: *urnetwork-common
    container_name: urfix-2
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-2
      - URNETWORK_AUTH_CODE=AUTH_CODE_2
    volumes:
      - ur_config_2:/root/.urnetwork
      - urfix-2_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9002:8080"

  node-3:
    <<: *urnetwork-common
    container_name: urfix-3
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-3
      - URNETWORK_AUTH_CODE=AUTH_CODE_3
    volumes:
      - ur_config_3:/root/.urnetwork
      - urfix-3_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9003:8080"

  node-4:
    <<: *urnetwork-common
    container_name: urfix-4
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-4
      - URNETWORK_AUTH_CODE=AUTH_CODE_4
    volumes:
      - ur_config_4:/root/.urnetwork
      - urfix-4_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9004:8080"

  node-5:
    <<: *urnetwork-common
    container_name: urfix-5
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-5
      - URNETWORK_AUTH_CODE=AUTH_CODE_5
    volumes:
      - ur_config_5:/root/.urnetwork
      - urfix-5_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9005:8080"

  node-6:
    <<: *urnetwork-common
    container_name: urfix-6
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-6
      - URNETWORK_AUTH_CODE=AUTH_CODE_6
    volumes:
      - ur_config_6:/root/.urnetwork
      - urfix-6_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9006:8080"

  node-7:
    <<: *urnetwork-common
    container_name: urfix-7
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-7
      - URNETWORK_AUTH_CODE=AUTH_CODE_7
    volumes:
      - ur_config_7:/root/.urnetwork
      - urfix-7_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9007:8080"

  node-8:
    <<: *urnetwork-common
    container_name: urfix-8
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-8
      - URNETWORK_AUTH_CODE=AUTH_CODE_8
    volumes:
      - ur_config_8:/root/.urnetwork
      - urfix-8_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9008:8080"

  node-9:
    <<: *urnetwork-common
    container_name: urfix-9
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-9
      - URNETWORK_AUTH_CODE=AUTH_CODE_9
    volumes:
      - ur_config_9:/root/.urnetwork
      - urfix-9_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9009:8080"

  node-10:
    <<: *urnetwork-common
    container_name: urfix-10
    environment:
      - BUILD=jwt
      - ENABLE_VNSTAT=true
      - HOST_HOSTNAME=${HOSTNAME}
      - URNETWORK_NODE_NAME=urfix-10
      - URNETWORK_AUTH_CODE=AUTH_CODE_10
    volumes:
      - ur_config_10:/root/.urnetwork
      - urfix-10_vnstat:/var/lib/vnstat
      - ./proxy.txt:/app/proxy.txt
    ports:
      - "9010:8080"

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
  urfix-1_vnstat:
  urfix-2_vnstat:
  urfix-3_vnstat:
  urfix-4_vnstat:
  urfix-5_vnstat:
  urfix-6_vnstat:
  urfix-7_vnstat:
  urfix-8_vnstat:
  urfix-9_vnstat:
  urfix-10_vnstat:
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

Check your Client Manager. You should see one entry per node, identified by the names in `URNETWORK_NODE_NAME` and a redacted public IP for privacy, for example:

```text
urfix-1 @ 69.x.x.96 [v2026.x]
urfix-2 @ 69.x.x.96 [v2026.x]
```