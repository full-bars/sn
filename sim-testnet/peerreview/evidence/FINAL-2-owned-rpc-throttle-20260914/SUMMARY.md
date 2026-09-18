# Owned-RPC ingress correction and second refusal

Frozen xops candidate `51325d00e18a157b2ceb5d5ac44bb0645550b289` removes the source-template request and per-client connection throttles on both owned archive and lightnode RPC ingress routes. It removes the corresponding obsolete variables and assertions while retaining exact binds, allowlists, GET/POST controls, loopback backends, WebSocket forwarding, health paths, body bounds, timeouts, and node/global resource bounds.

The existing 29-method `test_subtensor_playbook.py` module passed using the pinned Python/Jinja environment. Its two new deterministic renderer roots passed in the full module and in two fresh focused processes. Restoring only the old nginx template in a disposable worktree produced the expected one throttle failure and one passing route-preservation control.

A strict Jinja render from current candidate vars produced `render/nginx.conf` with SHA-256 `6371976bddf174874083f22897bb072977c5218f7b161a95f1e1c02a13edd453`. It contains no `limit_req` or `limit_conn` directive and preserves both exact-address route pairs, access controls, and loopback backends. Local nginx was unavailable, so no `nginx -t` was attempted.

The second 7eab native attempt remains a closed failure: it exited 1 at `2026-09-14T16:09:25.467328347Z` on `fleet.renew.1.31.bind.4` with the retained HTTP 429 message after marker `2400/3455`. Its six state fingerprints were unchanged, no journal rows or transactions were added, and no repair was submitted. The first-429 transport bundle is intentionally not modified or duplicated here.

The source correction is not deployed. Actual host configuration, the exact limiter kind that rejected either call, and rollout success remain unverified because the default SSH public key was denied and no local nginx binary was available.
