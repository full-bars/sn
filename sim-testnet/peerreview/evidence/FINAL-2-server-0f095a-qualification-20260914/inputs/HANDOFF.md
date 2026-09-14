# Frozen server0f execution owners

Prepared by Astra; Terra alone executes. No Go, formatter, compiler, test,
service or replay command was executed during preparation. All twelve shell
files passed `bash -n`. Source and prior captures were not changed.

The controller normal/race owners are ready for immediate independent launch
with `bash /absolute/path` and `login:false`. Startifact normal/race owners
are also independent and service-free. Every owner declares GOMAXPROCS=4 and
build/test parallelism4; Terra owns aggregate CPU admission.

Exact cohort normal p2 requires completed successful controller25 normal p1;
p3 requires completed successful p2. Both reuse the exact retained p1 normal
binary and its SHA receipt, with a new private PostgreSQL/Redis service owner
per invocation. They never rebuild or reuse test results.

Commands preserve existing production deadlines and HANDOFF scopes: startifact
49 roots in normal/race, controller25 roots in normal/race, then two fresh
sequential executions of the original normal cohort root. The nine existing
startifact replica subtests are explicitly retained in the PASS outcome table.

Each owner uses the existing source wrapper, qualified Go helper fences before
and after all13 repositories, unchanged 12 non-server heads from the e3d graph,
and server0f with its fresh manifest. Each retains source heads, full fence
outputs, native compile/list/body commands and exits, exact compiled root
comparison, test binary SHA/mode/size, native body timestamps, raw stdout/stderr,
existing Go event-verifier results, and actual service cleanup/outer exits.
A pre-body refusal remains exit125 in unexecuted stages.

The normal/race binaries use the completed diagnostic's warmed content-addressed
GOCACHE and the prepared modcache-v2. Reusing compiler cache does not reuse test
results; each native body has `-test.count=1`. The diagnostic's source, raw
streams, profile and terminal receipts remain retained.

Build/list budget1200s, existing native test budgets300/600s for startifact and
600/900s for controller, outer native body budget plus60s. No production30s
deadline, population, immutable write/readback, quota or cancellation rule changed.
Actual testnet RPC is not used by these private fixture tests.

## startifact49-normal-p1

```sh
bash /home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/commands/capture-startifact49-normal-p1.sh
```

Entry SHA-256: `beec62cc979e747159faa55d41f1c9f426efef4924ae202c5cf1ccf4db1e1687`.
Body SHA-256: `db20d64ae52549d0bd97059d29a305bb9e7425ee3112c1ac7f468ec656211a73`.
Capture: `/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/startifact49-normal-p1`.

## startifact49-race-p1

```sh
bash /home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/commands/capture-startifact49-race-p1.sh
```

Entry SHA-256: `9685accd9ad58d6b7a9a126bcae7e560dfa263be2fe3cb292efa9cbe4260c4e1`.
Body SHA-256: `476a2ea0303cb1221a7afd82414a3dfbfdec18cf98c64a86e1ec8eb2f85ed5d6`.
Capture: `/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/startifact49-race-p1`.

## controller25-normal-p1

```sh
bash /home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/commands/capture-controller25-normal-p1.sh
```

Entry SHA-256: `2843856d625217d3ff7ef58d2df38b4878d1a94613bdc9fff8f683785ab524d5`.
Body SHA-256: `b7165da30b097bfa265b19f7c2986e45d973a0471b3d3015065d0b7f1c8ff846`.
Capture: `/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/controller25-normal-p1`.

## controller25-race-p1

```sh
bash /home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/commands/capture-controller25-race-p1.sh
```

Entry SHA-256: `18c0d959ac4faee21e04affbb5c5707aaf5741646bc962d520c81af17badf34f`.
Body SHA-256: `83f696ad5d74480cfc3d2b6f2ee83a483983a9a0c09e6d00ce1f36f6eb6e42a9`.
Capture: `/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/controller25-race-p1`.

## cohort1-normal-p2

```sh
bash /home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/commands/capture-cohort1-normal-p2.sh
```

Entry SHA-256: `79524ccbfa83a81ea587d7ca16b6ae278ccd0fd8512ea029e71103a9db1e24a2`.
Body SHA-256: `531837914c56170d6261b3d9b8706c891ec2d1a4dc704b4e17bf594e01a04efd`.
Capture: `/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/cohort1-normal-p2`.

## cohort1-normal-p3

```sh
bash /home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/commands/capture-cohort1-normal-p3.sh
```

Entry SHA-256: `e08e19b9aa6268c5cba408a4a098b820213761db8303fb3ec7572fffe703ce20`.
Body SHA-256: `3a77f00c5c3d04ac1d0be80bde4fd181721a878727a530b30c2507b9b5713eed`.
Capture: `/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/cohort1-normal-p3`.

Common input manifest: `/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/inputs.sha256`.
SHA-256: `114191cb18e4b77a7a8f9597c6dd14f25dc0773fb2c1cf8853cebbc584ca1f0c`.
