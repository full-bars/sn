# Pre-build admission refusal

- Captured: 2026-09-14 UTC before any `go build` process was invoked.
- Attempted capture layout: `final-cli-fc908685`.
- Refusal: the inherited final-CLI template checked `sn/release.lock.yml`; the published release projection stores the identical reviewed lock at `sn/deploy/testnet/release.lock.yml`.
- The published source is also a valid Git worktree, whose `sn/.git` is a regular gitdir pointer file. The inherited template's directory-only `.git` predicate is therefore not applicable.
- The source-pair snapshot was created while all thirteen resolved repository inputs were clean. No compiler, test, native command, RPC call, or source mutation was admitted.
- The corrected capture is separately named `final-cli-fc908685-r1`; it records logical and resolved physical paths for the declared `vault` projection and accepts an existing `.git` file or directory.
