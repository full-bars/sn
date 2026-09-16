# Security Review Report: Provider Package

## Critical Findings

### 1. Race Condition: `proxyIndexByAddr` Global Map Access
**File:** `provider/provide.go`, lines 15-28

```go
var proxyIndexByAddr sync.Map

func setProxyIndex(addr string, idx int) {
    proxyIndexByAddr.Store(addr, idx)
}

func getProxyIndex(addr string) int {
    if v, ok := proxyIndexByAddr.Load(addr); ok {
        return v.(int)
    }
    return 0
}
```

**Issue:** While `sync.Map` provides thread-safe operations, the `getProxyIndex` function silently returns `0` when an address isn't found. This creates a **silent failure mode** where callers may use index `0` for an unregistered proxy, potentially routing traffic to the wrong proxy or causing undefined behavior.

**Risk:** Medium - Silent fallback to index 0 could cause traffic misrouting.

**Recommendation:** Return an error or use a sentinel value (-1) for missing entries. Log a warning when falling back.

---

### 2. Race Condition: `cancelSource` Atomic Value with Potential Data Race
**File:** `provider/provide.go`, lines 45-48, 130-135

```go
cancelSource         atomic.Value // stores string(debug.Stack())

st.cancel = func() {
    if st.cancelSource.Load() == nil {
        st.cancelSource.Store(string(debug.Stack()))
    }
    st.rawCancel()
}
```

**Issue:** The check-then-act pattern (`Load()` then `Store()`) is not atomic. Multiple goroutines calling `st.cancel()` simultaneously could both pass the `nil` check and both call `Store()`, resulting in a race condition where the stored stack trace may not be from the first cancellation.

**Risk:** Low-Medium - Debug information may be incorrect, but the actual cancellation (`rawCancel()`) is safe.

**Recommendation:** Use `CompareAndSwap` or `LoadOrStore` for atomic check-and-set:
```go
st.cancelSource.CompareAndSwap(nil, string(debug.Stack()))
```

---

### 3. Resource Leak: `hotSwapIPC` Deferred Close in Wrong Scope
**File:** `provider/provide.go`, lines 104-108

```go
if ipcFile, isChild := getHotSwapChildIPC(); isChild {
    st.isHotSwapCandidate = true
    metricsHandoffPending.Store(true)
    st.hotSwapIPC = ipcFile
    defer st.hotSwapIPC.Close()
    if err := runHotSwapChildHandshake(st.hotSwapIPC, st.opts, st.apiUrl); err != nil {
        tlog("[hotswap] Candidate pre-flight failed: %v\n", err)
        os.Exit(2)
    }
}
```

**Issue:** The `defer st.hotSwapIPC.Close()` is inside `provideSetupMemory()`, but `st.hotSwapIPC` is stored in the shared state. If the handshake fails and `os.Exit(2)` is called, deferred functions won't run, leaking the IPC file descriptor.

**Risk:** Medium - File descriptor leak on error path, potential resource exhaustion in repeated hot-swap attempts.

**Recommendation:** Explicitly close before `os.Exit(2)`:
```go
if err := runHotSwapChildHandshake(st.hotSwapIPC, st.opts, st.apiUrl); err != nil {
    st.hotSwapIPC.Close()
    tlog("[hotswap] Candidate pre-flight failed: %v\n", err)
    os.Exit(2)
}
```

---

### 4. Missing Error Check: `os.Hostname()` Error Ignored
**File:** `provider/provide.go`, line 111

```go
host, _ := os.Hostname()
```

**Issue:** The error from `os.Hostname()` is silently ignored. If hostname resolution fails, `host` will be empty, resulting in incomplete or misleading log entries.

**Risk:** Low - Logging only, but could hamper debugging.

**Recommendation:** Log the error or use a fallback:
```go
host, err := os.Hostname()
if err != nil {
    host = "unknown"
    tlog("[startup] failed to get hostname: %v\n", err)
}
```

---

### 5. Security Issue: JWT File Read Without Permission Validation
**File:** `provider/provide.go`, lines 120-135

```go
home, _ := os.UserHomeDir()
if home != "" {
    jwtPath := filepath.Join(home, ".urnetwork", "jwt")
    if _, err := os.Stat(jwtPath); err == nil {
        if jwtBytes, err := os.ReadFile(jwtPath); err == nil {
            if exp := parseJWTExpiryTime(string(jwtBytes)); exp != nil {
                // ... log expiry
            }
        }
    }
}
```

**Issue:** 
1. No check that the JWT file has restrictive permissions (should be 0600)
2. No validation of JWT content before parsing
3. Error from `os.UserHomeDir()` ignored

**Risk:** High - JWT tokens are sensitive credentials. If the file has overly permissive permissions (e.g., 0644), other users on the system could read the token.

**Recommendation:** 
- Check file permissions and warn if not 0600
- Validate JWT format before parsing
- Handle `os.UserHomeDir()` error

---

### 6. Stub Functions Silently Returning Zero/Nil in Production Paths
**File:** `provider/main_stubs.go`, lines 15-30

```go
func getProxyIndex(addr string) int {
    if v, ok := proxyIndexByAddr.Load(addr); ok {
        return v.(int)
    }
    return 0  // Silent fallback
}

func proxyHealthByAddressV2026() map[string]ProxyHealthStatus { 
    return ProxyHealthByAddress()  // May return nil
}

func unregisterProxyStub(idx interface{}) { 
    if i, ok := idx.(int); ok { 
        UnregisterProxy(i) 
    }
    // Silently ignores non-int indices
}
```

**Issue:** These stubs silently return zero values or ignore errors, which can mask critical failures in production. The `unregisterProxyStub` silently ignores invalid indices, potentially leaving proxies registered.

**Risk:** High - Silent failures in proxy management could lead to traffic routing to dead proxies or resource leaks.

**Recommendation:** 
- Add logging for unexpected conditions
- Return errors where appropriate
- Add metrics counters for stub fallback paths

---

### 7. Context Cancellation: `defer st.cancel()` in `provideSetupSignals`
**File:** `provider/provide.go`, lines 127-131

```go
st.cancel = func() {
    if st.cancelSource.Load() == nil {
        st.cancelSource.Store(string(debug.Stack()))
    }
    st.rawCancel()
}
defer st.cancel()
```

**Issue:** The `defer st.cancel()` is in `provideSetupSignals()`, not in the main `provide()` function. If `provideSetupSignals()` returns normally, the context is cancelled immediately, potentially affecting goroutines that expect the context to remain active.

**Risk:** High - Premature context cancellation could cause unexpected shutdown of goroutines.

**Recommendation:** Move the `defer st.cancel()` to the main `provide()` function or ensure it's only called on actual shutdown.

---

### 8. Missing Error Handling: `startControlSocket` Error Path
**File:** `provider/provide.go`, lines 145-152

```go
st.cleanupControlSocket, err = startControlSocket(st.ctx, globalControlState)
if err != nil {
    tlog("[control] failed to start control socket, urnet-tools will fall back to file-based overrides: %s\n", err)
} else {
    defer func() {
        if st.cleanupControlSocket != nil {
            st.cleanupControlSocket()
        }
    }()
}
```

**Issue:** When `startControlSocket` fails, the system falls back to file-based overrides but doesn't set up any monitoring or notification mechanism. This could lead to silent configuration drift.

**Risk:** Medium - Operational issue where control commands may not be applied.

**Recommendation:** Add a periodic check for file-based overrides when socket fails, or set up a file watcher.

---

### 9. Race Condition: `proxyCancelMap` Access Pattern
**File:** `provider/provide.go`, lines 49-51

```go
proxyCancelMu        sync.Mutex
proxyCancelMap       map[string]context.CancelFunc
```

**Issue:** While the mutex protects the map, there's no documentation about the locking pattern. If any code accesses `proxyCancelMap` without holding `proxyCancelMu`, it will cause a race condition.

**Risk:** Medium - Potential data race if locking discipline is not maintained.

**Recommendation:** Add helper methods for map access that encapsulate locking, and document the locking requirements.

---

### 10. Resource Leak: `st.wg.Wait()` Without Timeout
**File:** `provider/provide.go`, lines 58-60

```go
st.wg.Wait()
```

**Issue:** The main function waits indefinitely for all goroutines to complete. If any goroutine hangs or deadlocks, the process will never exit, requiring manual intervention.

**Risk:** Medium - Potential for hung processes in production.

**Recommendation:** Add a timeout or health check mechanism:
```go
done := make(chan struct{})
go func() {
    st.wg.Wait()
    close(done)
}()
select {
case <-done:
    // Normal exit
case <-time.After(30 * time.Second):
    tlog("[provider] timeout waiting for goroutines, forcing exit\n")
    // Force exit or dump goroutine stacks
}
```

---

## Summary

| Severity | Count | Key Areas |
|----------|-------|-----------|
| Critical | 0 | - |
| High | 3 | JWT permissions, stub functions, context cancellation |
| Medium | 5 | Race conditions, resource leaks, error handling |
| Low | 2 | Logging issues, minor error handling |

**Priority Actions:**
1. Add JWT file permission validation
2. Replace silent stub fallbacks with explicit error handling
3. Fix context cancellation scope
4. Add timeouts to `WaitGroup.Wait()`
5. Implement atomic check-and-set for `cancelSource`