 # H3 Provider Parity Review — 2026-09-16_13:54 UTC

## Summary

The h3-provider migration is currently **not at parity** with the SN fork. The project reportedly builds and passes its remaining tests, but the port is missing 85 test files, three non-test files, and contains 34 stubs across critical paths. Several large diffs show intentional changes that remove or replace production behavior, and some of those changes contradict the stated v2026 connect API surface.

The build-clean/test-pass claim should be treated as **“the current subset of code compiles”**, not as evidence of feature parity.

---

## Missing Files

### 85 test files
- No SN fork test files were ported.
- This removes regression coverage for control sockets, hotswap, renewal, DoH cache, resource pressure, metrics, and subnet RPC.
- “Tests pass” is not meaningful for parity when the test suite itself was not ported.

### `main.go`
- The SN fork entrypoint was not ported.
- `control_socket.go` was changed to `package main`, but without a `main()` function the binary cannot be built as a runnable provider.
- If another file provides `func main`, that file is not shown in the diff and should be identified.
- If no other file provides `func main`, this is a release-blocking gap.

### `read_fd_frac_unix.go` / `read_fd_frac_windows.go`
- These platform-specific files are missing.
- `control_socket.go` now calls `restrictSocketACL(path)`, which is likely platform-specific.
- If `restrictSocketACL` is defined in the missing `read_fd_frac_*` files, the build is not actually clean on all target platforms.
- The Windows variant is especially important because the diff notes that `os.Chmod(0600)` does not secure NTFS permissions.

---

## Key Diffs and Risks

### `control_socket.go`

- **Package change**: `package provider` → `package main`.
  - If other files in `provider/` remain `package provider`, the build is invalid.
  - If all files were converted to `package main`, then `main.go` is still missing.

- **Removed `Version` and `controlLog`**:
  - `Version` was replaced by something else or dropped.
  - `controlLog` was replaced with `tlog`.
  - Need to verify `tlog` is defined in all build configurations and is not a stub.

- **Added `restrictSocketACL(path)`**:
  - Good security hardening, but the function is not shown in the diff.
  - The call as pasted is syntactically invalid:
    ```go
    if err := restrictSocketACL(path); return nil, fmt.Errorf("control socket ACL: %w", err)
    ```
  - This is likely a diff artifact, but it should be verified.

- **Accept loop logging**:
  - `controlLog` → `tlog` changes log output but not control flow.

### `sn.go`

- Removed `DefaultApiUrl`.
- Removed `providerStateDir` / state path helper.
- Removed `bandwidth` import.
- Removed large adaptation notes.
- 12 stubs remain in this file.

**Risk**: Any code that referenced `DefaultApiUrl` or `providerStateDir` will break unless those symbols were moved elsewhere. The diff does not show where they were moved.

### `renewal_watcher.go`

- Removed `RenewalOOB`, `NewRenewalOOB`, `WrapRenewalOOB`.
- Removed 401 audit counters and `on401` callback.
- Removed `renewalLog`.
- Removed `jwt`, `protocol`, `sync/atomic`, and `strings` imports.

**Risk**: The SN fork added 401 interception to `connect.ApiOutOfBandControl`. The h3-provider removes it. If v2026 connect does not support 401 audit methods, this is an intentional adaptation, but it means the provider no longer reacts to 401s the same way. Proxy JWT renewal behavior may regress.

### `hotswap.go`

- Removed `