package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// proxyIndexByAddr maps proxy addresses to their stable integer IDs.
// Needed because the new connect.ProxySettings doesn't carry an Index field
// (the old fork added it). Populated in provideLauncherLoop and read in
// provideWithProxy.
var proxyIndexByAddr sync.Map

func setProxyIndex(addr string, idx int) {
	proxyIndexByAddr.Store(addr, idx)
}

// deleteProxyIndex removes a proxy address from the index map.
// Called from UnregisterProxy to prevent unbounded growth.
func deleteProxyIndex(addr string) {
	proxyIndexByAddr.Delete(addr)
}

// getProxyIndex returns the stable integer ID for a proxy address,
// or -1 if the address was never registered. Callers must check for
// -1 to avoid misattributing health metrics to the direct proxy (index 0).
func getProxyIndex(addr string) int {
	if v, ok := proxyIndexByAddr.Load(addr); ok {
		return v.(int)
	}
	return -1
}

// provideState holds the shared mutable state used by provide() and its
// sub-functions.
type provideState struct {
	opts                 docopt.Opts
	apiUrl               string
	connectUrl           string
	maxMemory            connect.ByteCount
	nodeName             string
	isHotSwapCandidate   bool
	hotSwapIPC           io.ReadWriteCloser
	candidateAckOnce     sync.Once
	cleanupControlSocket func()
	ctx                  context.Context
	cancel               context.CancelFunc
	cancelSourceOnce     sync.Once
	cancelSourceVal      string // protected by cancelSourceOnce
	rawCancel            context.CancelFunc
	wg                   sync.WaitGroup
	proxyCancelMu        sync.Mutex
	proxyCancelMap       map[string]context.CancelFunc
}

// provide is the main entry point for the provider process.
func provide(opts docopt.Opts) {
	st := &provideState{}
	st.opts = opts
	st.proxyCancelMap = make(map[string]context.CancelFunc)

	provideSetupMemory(st)
	provideSetupSignals(st)
	defer st.cancel()
	defer flushRetentionEvents()
	provideLaunchGoroutines(st)
	defer provideLauncherLoop(st)()
	provideStatusServer(st)

	<-st.ctx.Done()
	done := make(chan struct{})
	go func() {
		st.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		tlog("[provider] timed out waiting for goroutines to exit\n")
	}

	tlog("[provider] exiting\n")
	critLog("PROVIDER EXIT: normal shutdown (code=0)")
	closeAllCaches(st)
}

// provideSetupMemory handles memory limits, identity staging, hot-swap
// candidate checks, and JWT status logging.
func provideSetupMemory(st *provideState) {
	apiUrl, err := resolveApiUrl(st.opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}
	st.apiUrl = apiUrl

	connectUrl, err := resolveConnectUrl(st.opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}
	st.connectUrl = connectUrl

	maxMemoryHumanReadable, err := st.opts.String("--max-memory")
	var maxMemory connect.ByteCount
	if err == nil {
		maxMemory, err = connect.ParseByteCount(maxMemoryHumanReadable)
		if err != nil {
			panic(fmt.Errorf("Bad mem argument: %s", maxMemoryHumanReadable))
		}
	}
	if 0 < maxMemory {
		// REMOVED: ResizeMessagePoolsPerClass(maxMemory / 8)
		// v2026 connect owns pool sizing via its memory_budget package.
		// Only the GOMEMLIMIT (line below) is needed from our side.
		debug.SetMemoryLimit(maxMemory)
	}
	st.maxMemory = maxMemory
	applyPoolAutoSize(maxMemory)

	provideStartTime = time.Now()

	if os.Getenv(EnvHotSwap) != "1" {
		applyStagedSession()
	}

	// HotSwap Candidate check
	if ipcFile, isChild := getHotSwapChildIPC(); isChild {
		st.isHotSwapCandidate = true
		metricsHandoffPending.Store(true)
		st.hotSwapIPC = ipcFile
		// NOTE: IPC handle lifecycle is managed by the hotswap parent/child
		// handoff in provideWithProxy (candidate ACK), not here.
		if err := runHotSwapChildHandshake(st.hotSwapIPC, st.opts, st.apiUrl); err != nil {
			tlog("[hotswap] Candidate pre-flight failed: %v\n", err)
			st.hotSwapIPC.Close()
			os.Exit(2)
		}
	}

	if !st.isHotSwapCandidate {
		finishIdentity := bannerPhase("Identity")
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "unknown"
		}
		critLog("STARTUP: version=%s pid=%d host=%s", RequireVersion(), os.Getpid(), host)
		if VersionStamp != "" {
			tlog("[startup] version stamp: %s\n", VersionStamp)
		}
		finishIdentity(RequireVersion())
	} else {
		tlog("[hotswap] Candidate PID %d promoted to live provider (version=%s)\n", os.Getpid(), RequireVersion())
	}

	// Log JWT expiry status at startup
	home, _ := os.UserHomeDir()
	if home != "" {
		jwtPath := filepath.Join(home, ".urnetwork", "jwt")
		if info, err := os.Stat(jwtPath); err == nil {
			if perm := info.Mode().Perm(); perm != 0600 {
				tlog("[jwt] warn: %s has permissions %04o (expected 0600)\n", jwtPath, perm)
			}
			if jwtBytes, err := os.ReadFile(jwtPath); err == nil {
				if exp := parseJWTExpiryTime(string(jwtBytes)); exp != nil {
					remaining := time.Until(*exp)
					daysUntil := remaining.Hours() / 24
					if daysUntil >= 1 {
						tlog("[jwt] expires in %d days\n", int(daysUntil))
					} else if daysUntil >= 0 {
						tlog("[jwt] expires in %s\n", formatDuration(remaining))
					} else {
						tlog("[jwt] EXPIRED %s ago — refresh needed\n", formatDuration(-remaining))
					}
				}
			}
		}
	}
}

// provideSetupSignals creates the root context, signal handling, hot-swap
// listener, control socket, and control state loading.
func provideSetupSignals(st *provideState) {
	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	rawCtx, rawCancel := context.WithCancel(event.Ctx())
	st.ctx = rawCtx
	st.rawCancel = rawCancel

	st.cancel = func() {
		st.cancelSourceOnce.Do(func() {
			st.cancelSourceVal = string(debug.Stack())
		})
		st.rawCancel()
	}

	if !st.isHotSwapCandidate {
		startHotSwapSignalListener(st.ctx, st.cancel, st.opts)
		hotSwapTrigger = func() error {
			return runHotSwapParentHandoff(st.ctx, st.cancel, st.opts)
		}
	}

	// NOTE: flushRetentionEvents is deferred in provide() scope, not here,
	// so it runs at process shutdown rather than when provideSetupSignals returns.

	finishControl := bannerPhase("Control state")
	if loaded, err := loadControlState(); err != nil {
		tlog("[control] failed to load provider_state.json, starting with no socket-set overrides: %s\n", err)
		finishControl("loaded (no overrides)", "⚠")
	} else {
		globalControlState = loaded
		finishControl("loaded")
	}
	mergePendingOverrides(globalControlState)
	applyPersistedRuntimeTuning(globalControlState)
	initPersistentErrors()
	initAuditRing()
	// A Docker in-place execve successor is not a candidate process (no
	// IPC descriptor) but the env marker survives the exec, so both kinds
	// of handoff successor get labelled hotswap. The persist gate is armed
	// only for spawned candidates: the Docker successor's ring loaded
	// after the parent's pre-exec flush, so it persists immediately.
	recordProcessStart(st.isHotSwapCandidate || os.Getenv(EnvHotSwapExec) == "1", st.isHotSwapCandidate)
	globalControlState.shutdownFn = st.cancel

	if !st.isHotSwapCandidate {
		var err error
		st.cleanupControlSocket, err = startControlSocket(st.ctx, globalControlState)
		if err != nil {
			tlog("[control] failed to start control socket, urnet-tools will fall back to file-based overrides: %s\n", err)
		} else {
			// The hotswap commit point quiesces this socket so no command
			// accepted after the audit flush can be lost in the parent's
			// memory mid-handoff. The wrapper nils the closure on the way
			// out so a later graceful exit cannot clean up again and delete
			// the successor's freshly bound socket.
			setQuiesceHook(func() {
				if st.cleanupControlSocket != nil {
					st.cleanupControlSocket()
					st.cleanupControlSocket = nil
				}
			})
			// NOTE: Control socket cleanup is handled by RegisterCoordinatorCloser
			// (below) and by closeAllCaches() in provide()'s shutdown path.
			// Do NOT defer unregSocketCloser here — the closer must stay
			// registered for the lifetime of the process.
			RegisterCoordinatorCloser(func() {
				if st.cleanupControlSocket != nil {
					st.cleanupControlSocket()
					st.cleanupControlSocket = nil
					setQuiesceHook(nil)
				}
			})
		}
	}

	if !st.isHotSwapCandidate {
		if err := notifySystemdReady(); err != nil {
			tlog("[systemd] READY=1 notify failed: %s\n", err)
		}
		reportProxyStatusToSystemd()
	}

	go func() {
		<-st.ctx.Done()
		source := st.cancelSourceVal
		if source == "" {
			source = "context cancelled by parent (signal or event.Set())"
		}
		tlog("[provider] shutting down: main context cancelled\n")
		critLog("SIGNAL: context cancelled — draining goroutines\nsource:\n%s", source)
	}()
}

// provideLaunchGoroutines starts all background monitoring and reporting.
func provideLaunchGoroutines(st *provideState) {
	// Subnet claim wallet
	if coldkeySs58, walletErr := st.opts.String("--wallet"); walletErr == nil && coldkeySs58 != "" {
		walletClientStrategy := connect.NewClientStrategyWithDefaults(st.ctx)
		if err := snSetWallet(st.ctx, walletClientStrategy, st.apiUrl, coldkeySs58); err != nil {
			fmt.Printf("subnet wallet not set: %s\n", err)
			fmt.Printf("continuing to provide. Retry with: provider wallet set <coldkey_ss58>\n")
		}
	}

	// Hourly pulse — diagnostic logging + stall-recovery check.
	// REMOVED: TriggerPulse() — was a no-op in v2026 connect which handles
	// stall recovery internally via its transport reconnection loop.
	go func() {
		for {
			select {
			case <-st.ctx.Done():
				return
			case <-time.After(1 * time.Hour):
				if ProxyHealthCount() > 0 {
					_, dead, degraded, _, connecting := ProxyHealthSnapshot()
					down := len(dead) + len(degraded)
					tlog("[hourly-maintenance] reconnecting stalled transports: down=%d dead=%d degraded=%d connecting=%d\n",
						down, len(dead), len(degraded), len(connecting))
				}
				// TriggerPulse() removed — v2026 connect handles stall recovery.
			}
		}
	}()

	st.nodeName = strings.TrimSpace(os.Getenv("URNETWORK_NODE_NAME"))

	watcherName := st.nodeName
	if watcherName == "" {
		var err error
		watcherName, err = os.Hostname()
		if err != nil || watcherName == "" {
			watcherName = "unknown"
		}
		if containerIDRe.MatchString(watcherName) {
			watcherName = "provider"
		}
	}

	st.wg.Add(1)
	go connect.HandleError(func() {
		defer st.wg.Done()
		runHealthHeartbeat(st.ctx, provideStartTime, os.Getenv("URNETWORK_PROFILE"))
	})
	st.wg.Add(1)
	go connect.HandleError(func() {
		defer st.wg.Done()
		runBandwidthReporter(st.ctx, watcherName, watcherName, os.Getenv("URNETWORK_REPORT_URL"), provideStartTime)
	})
	st.wg.Add(1)
	go connect.HandleError(func() {
		defer st.wg.Done()
		runHeartbeatReporter(st.ctx, watcherName, watcherName, os.Getenv("URNETWORK_REPORT_URL"), provideStartTime)
	})
	st.wg.Add(1)
	go connect.HandleError(func() {
		defer st.wg.Done()
		runJWTRefresher(st.ctx, st.apiUrl)
	})
	st.wg.Add(1)
	go connect.HandleError(func() {
		defer st.wg.Done()
		runEarningWindows(st.ctx)
	})
	go connect.HandleError(func() { runLifetimeCollector(st.ctx) })
	go connect.HandleError(func() { runProfitHeartbeat(st.ctx) })
	go connect.HandleError(func() { runBillableRateWriter(st.ctx) })
	go connect.HandleError(func() { runNodeSnapshotSampler(st.ctx) })

	go connect.HandleError(func() { paceMonitor(st.ctx) })
}

// provideWithProxy runs a single proxy's transport lifecycle.
func provideWithProxy(st *provideState, proxyCtx context.Context, proxySettings *connect.ProxySettings, isNative bool, isURLSourced bool) {
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ProxySettings = proxySettings

	// Compute proxy identity early so we can wire bandwidth tracking
	// before the client strategy (and its DialContext) is created.
	identityKey := "direct"
	proxyIndex := 0
	if proxySettings != nil {
		identityKey = proxySettings.Address
		proxyIndex = getProxyIndex(proxySettings.Address)
	}

	// Register bandwidth tracker for this proxy (for health/earnings reporting).
	// Wired at the LocalUserNat buffer settings below (the actual relay-egress
	// dial path), not here on clientStrategySettings — that only carries the
	// provider's own control-plane connection to the platform, not client
	// traffic. See bandwidth.WrapConnectSettings for why the naive
	// WrapDialContextSettings approach silently bypassed proxy routing.
	proxyBandwidth := RegisterProxyBandwidth(proxyIndex)

	clientSettings := connect.DefaultClientSettings()
	if seed, err := readProviderClientKeySeed(); err == nil && 0 < len(seed) {
		clientSettings.ClientKeySeed = seed
	} else if err != nil {
		tlog("[encryption] WARNING: could not read provider client key seed: %v — encryption will use a fresh identity\n", err)
	}
	if certPem, keyPem, err := readProviderTlsCertAndKey(); err == nil && 0 < len(certPem) && 0 < len(keyPem) {
		if clientSettings.EncryptionSettings == nil {
			clientSettings.EncryptionSettings = connect.DefaultEncryptionSettings()
		}
		clientSettings.EncryptionSettings.ProvideTlsCertificatePem = certPem
		clientSettings.EncryptionSettings.ProvideTlsPrivateKeyPem = keyPem
	} else if err != nil {
		tlog("[encryption] WARNING: could not read TLS cert/key: %v — encryption downgraded to opportunistic (no PQE)\n", err)
	} else {
		tlog("[encryption] WARNING: TLS cert/key not found on disk — encryption downgraded to opportunistic (no PQE)\n")
	}
	enableProviderEncryption(clientSettings)
	localUserNatSettings := connect.DefaultLocalUserNatSettings()

	// REMOVED: ApplyAutoTuning(clientSettings, localUserNatSettings)
	// The fork's ApplyAutoTuning auto-sized TCP/UDP buffers from system
	// resources. v2026 connect owns buffer sizing internally.
	applyLowmodeSettings(clientSettings, localUserNatSettings)
	applyTurboSettings(clientSettings, localUserNatSettings)

	profile := os.Getenv("URNETWORK_PROFILE")
	applyTurboMemoryLimit(profile, st.maxMemory)
	applyEcoSettings(st.maxMemory)
	ensureMemoryLimit(st.maxMemory)
	// Wrap the relay-egress ConnectSettings with byte counting. This is a
	// separate copy from clientStrategySettings.ConnectSettings on purpose —
	// wrapping in place would also instrument the provider's own
	// control-plane traffic to the platform API, double-counting it as
	// billable proxy bandwidth.
	relayConnectSettings := bandwidth.WrapConnectSettings(clientStrategySettings.ConnectSettings, proxyBandwidth, identityKey)
	localUserNatSettings.TcpBufferSettings.ConnectSettings = relayConnectSettings
	localUserNatSettings.UdpBufferSettings.ConnectSettings = relayConnectSettings
	remoteUserNatProviderSettings := connect.DefaultRemoteUserNatProviderSettings()

	clientStrategy := connect.NewClientStrategy(proxyCtx, clientStrategySettings)

	// Peer-client-key fetcher.
	if clientSettings.EncryptionSettings != nil && clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher == nil {
		clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId connect.Id) func(context.Context) ([]byte, error) {
			return func(fetchCtx context.Context) ([]byte, error) {
				r, err := connect.HttpGetWithStrategy(
					fetchCtx,
					clientStrategy,
					fmt.Sprintf("%s/key/%s", st.apiUrl, peerId),
					"",
					&connect.GetClientKeyResult{},
					connect.NewNoopApiCallback[*connect.GetClientKeyResult](),
				)
				if err != nil {
					return nil, err
				}
				return r.PublicKey, nil
			}
		}
	}

	// Auth loop with retry.
	byClientJwt, clientId, reused, err := func() (string, connect.Id, bool, error) {
		const provenMaxAuthFailures = 10
		const unprovenMaxAuthFailures = 3
		maxAuthFailures := provenMaxAuthFailures
		if proxySettings != nil && !globalProvenProxies.HasSucceeded(proxySettings.Address) {
			maxAuthFailures = unprovenMaxAuthFailures
		}
		authFailures := 0

		if proxySettings != nil && !isURLSourced {
			if globalProxySlowRetryState.Load().WasDropped(proxySettings.Address) || globalProxySlowRetryState.Load().TimeUntilNextAttempt(proxySettings.Address) > 0 {
				authFailures = maxAuthFailures
			}
		}
		for {
			var err error
			var byClientJwt string
			var clientId connect.Id
			var reused bool

			if proxySettings != nil && isURLSourced && !urlProxyPassesAdmission(proxyCtx, proxySettings.Address) {
				cfg := resolveProxyTableProbeConfig()
				if score, ok := cachedProxyURLScore(proxySettings.Address); ok && cfg.Enabled && score < cfg.PassBar {
					return "", connect.Id{}, false, fmt.Errorf("%w: %s (score %.2f)", errProxyURLBelowBar, proxySettings.Address, score)
				}
				err = fmt.Errorf("proxy unreachable: %s", proxySettings.Address)
			} else {
				err = func() error {
					admitFailureCount := authFailures
					if proxySettings != nil {
						admitFailureCount = globalProxyFailureHistory.FailureCount(proxySettings.Address)
					}
					release, waitErr := globalProxyAdmissionGate.Admit(proxyCtx, admitFailureCount)
					if waitErr != nil {
						return waitErr
					}
					defer release()

					if !isURLSourced && authFailures >= maxAuthFailures {
						select {
						case slowRetrySemaphore <- struct{}{}:
							defer func() { <-slowRetrySemaphore }()
						case <-proxyCtx.Done():
							return proxyCtx.Err()
						}
					}
					identityKey := "direct"
					if proxySettings != nil {
						identityKey = proxySettings.Address
					}
					byClientJwt, clientId, reused, err = provideAuth(proxyCtx, clientStrategy, st.apiUrl, st.opts, st.nodeName, identityKey)
					return err
				}()
				if proxySettings != nil {
					if err == nil {
						globalProvenProxies.MarkSucceeded(proxySettings.Address)
						globalProxyFailureHistory.Reset(proxySettings.Address)
						globalProxySlowRetryState.Load().ClearDropped(proxySettings.Address)
					}
					globalAuthRateLimiter.ReportResultForProxy(err, globalProvenProxies.HasSucceeded(proxySettings.Address))
				} else {
					globalAuthRateLimiter.ReportResult(err)
				}
				if err == nil {
					if reused {
						fmt.Printf("[reuse] client_id: %s\n", clientId)
					} else {
						fmt.Printf("[new] client_id: %s\n", clientId)
					}
					return byClientJwt, clientId, reused, nil
				}
			}

			if errors.Is(err, ErrTokenInvalid) {
				shmLogFatal(78, "token invalid or expired — exiting so the startup script can refresh it")
			}
			if strings.Contains(err.Error(), "Jwt does not exist") {
				authFailures = 0
				fmt.Printf("Authentication missing. Please run 'urnetwork auth' to configure your provider.\n")
				retryDelay := 30 * time.Second
				select {
				case <-proxyCtx.Done():
					return "", connect.Id{}, false, proxyCtx.Err()
				case <-time.After(retryDelay):
					continue
				}
			}

			authFailures++
			if proxySettings != nil {
				globalProxyFailureHistory.RecordFailure(proxySettings.Address)
				RecordProxyAuthFailure(proxyIndex, err)
			}
			if authFailures >= maxAuthFailures {
				cause := classifyAuthFailureCause(err)
				if isURLSourced {
					return "", connect.Id{}, false, fmt.Errorf("authentication failed after %d attempts — %s: %w", maxAuthFailures, cause, err)
				}
				if proxySettings != nil {
					startedAt := globalProxySlowRetryState.Load().RecordSlowRetryStart(proxySettings.Address)
					if globalProxySlowRetryState.Load().ShouldDrop(proxySettings.Address) {
						globalProxySlowRetryState.Load().MarkDropped(proxySettings.Address)
						dropAge := time.Since(startedAt)
						tlog("[proxy][slow-retry] proxy[%d] (%s) dropped after %s of continuous failure (%d total attempts)\n",
							getProxyIndex(proxySettings.Address), proxySettings.Address, formatDuration(dropAge), authFailures)
						var cancel context.CancelFunc
						st.proxyCancelMu.Lock()
						if proxyOwnsLaunch(proxyCtx, proxySettings.Address) {
							cancel = st.proxyCancelMap[proxySettings.Address]
							delete(st.proxyCancelMap, proxySettings.Address)
						}
						st.proxyCancelMu.Unlock()
						if cancel != nil {
							cancel()
						}
						return "", connect.Id{}, false, fmt.Errorf("proxy dropped after %s of continuous failure — %s", formatDuration(dropAge), cause)
					}
					slowRetryAttempt := authFailures - maxAuthFailures + 1
					if slowRetryAttempt > slowRetryRampAttempts && !globalProxySlowRetryState.Load().RecordSlowRetryAttempt(proxySettings.Address) {
						waitTime := globalProxySlowRetryState.Load().TimeUntilNextAttempt(proxySettings.Address)
						if waitTime <= 0 {
							waitTime = 24 * time.Hour
							tlog("[proxy][slow-retry] proxy[%d] (%s) waitTime was non-positive, clamping to %s\n",
								getProxyIndex(proxySettings.Address), proxySettings.Address, formatDuration(waitTime))
						}
						tlog("[proxy][slow-retry] proxy[%d] (%s) auth still failing after %d attempts (%s); next check in %s\n",
							getProxyIndex(proxySettings.Address), proxySettings.Address, authFailures, cause, formatDuration(waitTime))
						dailyTimer := time.NewTimer(waitTime)
						select {
						case <-proxyCtx.Done():
							dailyTimer.Stop()
							return "", connect.Id{}, false, proxyCtx.Err()
						case <-dailyTimer.C:
							continue
						}
					}
				}
				slowDelay := proxyAuthSlowRetryDelay(authFailures - maxAuthFailures + 1)
				if proxySettings != nil {
					tlog("[proxy][init] proxy[%d] (%s) auth still failing after %d attempts (%s); retrying in %s\n",
						getProxyIndex(proxySettings.Address), proxySettings.Address, authFailures, cause, formatDuration(slowDelay))
				} else if isNative {
					tlog("[proxy][init] proxy[0] (direct) auth still failing after %d attempts (%s); retrying in %s\n",
						authFailures, cause, formatDuration(slowDelay))
				} else {
					tlog("[init] auth still failing after %d attempts (%s); retrying in %s\n",
						authFailures, cause, formatDuration(slowDelay))
				}
				select {
				case <-proxyCtx.Done():
					return "", connect.Id{}, false, proxyCtx.Err()
				case <-time.After(slowDelay):
					continue
				}
			}

			retryDelay := proxyAuthRetryDelay(err, authFailures)
			if proxySettings != nil {
				tlog("[proxy][init] proxy[%d] (%s) auth failed (attempt %d/%d): %v. Will retry in %.2fs\n",
					getProxyIndex(proxySettings.Address), proxySettings.Address, authFailures, maxAuthFailures, err, float64(retryDelay/time.Millisecond)/1000.0)
			} else if isNative {
				tlog("[proxy][init] proxy[0] (direct) auth failed (attempt %d/%d): %v. Will retry in %.2fs\n",
					authFailures, maxAuthFailures, err, float64(retryDelay/time.Millisecond)/1000.0)
			} else {
				tlog("[init] auth failed (attempt %d/%d): %v. Will retry in %.2fs\n", authFailures, maxAuthFailures, err, float64(retryDelay/time.Millisecond)/1000.0)
			}
			select {
			case <-proxyCtx.Done():
				return "", connect.Id{}, false, proxyCtx.Err()
			case <-time.After(retryDelay):
			}
		}
	}()

	if err != nil {
		provideHandleAuthFailure(st, proxyCtx, proxySettings, isNative, isURLSourced, err)
		return
	}

	instanceId := connect.NewId()

	oob := connect.NewApiOutOfBandControl(proxyCtx, clientStrategy, byClientJwt, st.apiUrl)
	// Wrap OOB before handing it to the client so the connect library's
	// own OOB calls (heartbeat, contract) are intercepted for 401 audit.
	// This is the v2026 substitute for the fork's built-in Audit401Count
	// on connect.ApiOutOfBandControl.
	renewalOOB := WrapRenewalOOB(oob)
	connectClient := connect.NewClient(proxyCtx, clientId, renewalOOB, clientSettings)
	defer func() {
		unregisterEncryptionManager(connectClient.EncryptionSessionManager())
		connectClient.Close()
	}()
	registerEncryptionManager(connectClient.EncryptionSessionManager())

	// Persist live identity material.
	if !st.isHotSwapCandidate {
		if keyManager := connectClient.ClientKeyManager(); keyManager != nil {
			if seed := keyManager.Seed(); 0 < len(seed) {
				if err := writeProviderClientKeySeed(seed); err != nil {
					fmt.Printf("provider client key save failed: %s\n", err)
				}
			}
		}
		if encManager := connectClient.EncryptionSessionManager(); encManager != nil {
			certPem := encManager.ProvideTlsCertificatePem()
			keyPem := encManager.ProvideTlsPrivateKeyPem()
			if 0 < len(certPem) && 0 < len(keyPem) {
				if err := writeProviderTlsCertAndKey(certPem, keyPem); err != nil {
					fmt.Printf("provider tls cert/key save failed: %s\n", err)
				}
			}
		}
	}

	fmt.Printf("instance_id: %s\n", instanceId)

	auth := &connect.ClientAuth{
		ByJwt:      byClientJwt,
		InstanceId: instanceId,
		AppVersion: RequireVersion(),
	}
	// Wire UDP/QUIC bandwidth tracking via H3PacketConnFactory.
	// When set, the platform transport calls this instead of raw net.ListenUDP,
	// allowing us to wrap the PacketConn with byte counters.
	var platformSettings *connect.PlatformTransportSettings
	if proxyBandwidth != nil {
		platformSettings = connect.DefaultPlatformTransportSettings()
		platformSettings.H3PacketConnFactory = func(ctx context.Context) (net.PacketConn, error) {
			raw, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
			if err != nil {
				return nil, err
			}
			return bandwidth.NewPacketConn(raw, proxyBandwidth, identityKey), nil
		}
	}
	platformTransport := connect.NewPlatformTransport(proxyCtx, clientStrategy, connectClient.RouteManager(), st.connectUrl, auth, platformSettings)
	unregCloser := RegisterCoordinatorCloser(func() {
		platformTransport.Close()
	})
	defer unregCloser()

	proxyBecameLive()
	markProxyUp(proxyIndex)
	defer proxyWentDown()
	defer markProxyDown(proxyIndex)

	// HotSwap candidate ACK
	var unregSocketCloser func()
	st.candidateAckOnce.Do(func() {
		if st.isHotSwapCandidate && st.hotSwapIPC != nil {
			_ = runHotSwapChildAck(st.hotSwapIPC) // fire-and-forget IPC ack
			_ = st.hotSwapIPC.Close()
			startHotSwapSignalListener(st.ctx, st.cancel, st.opts)
			hotSwapTrigger = func() error {
				return runHotSwapParentHandoff(st.ctx, st.cancel, st.opts)
			}

			waitForControlSocketRelease(HotSwapAckTimeout + 15*time.Second)

			if reloaded, err := loadControlState(); err != nil {
				tlog("[control] candidate failed to reload provider_state.json on takeover: %s\n", err)
			} else {
				globalControlState.replaceAllWithMeta(reloaded.values, reloaded.meta)
			}
			mergePendingOverrides(globalControlState)
			applyPersistedRuntimeTuning(globalControlState)

			// The parent flushed its final audit entries before the
			// takeover message; this ring was loaded at spawn time and
			// predates that write, so pull them in now. Without this the
			// handoff event and the last control-socket commands would
			// exist only on disk and be dropped by the next persist.
			mergeAuditRingFromDisk()

			startMetricsAfterTakeover(globalControlState)

			if cleanup, err := startControlSocket(st.ctx, globalControlState); err != nil {
				tlog("[control] candidate failed to start control socket on takeover: %s\n", err)
				// Do not carry the parent's socket closer into the hotswap
				// commit point: this process never bound that socket.
				setQuiesceHook(nil)
			} else {
				st.cleanupControlSocket = cleanup
				// Same nil-out wrapper as the parent path: quiesce once,
				// then this process's socket is no longer ours to remove.
				setQuiesceHook(func() {
					if st.cleanupControlSocket != nil {
						st.cleanupControlSocket()
						st.cleanupControlSocket = nil
					}
				})
				unregSocketCloser = RegisterCoordinatorCloser(func() {
					if st.cleanupControlSocket != nil {
						st.cleanupControlSocket()
						st.cleanupControlSocket = nil
						setQuiesceHook(nil)
					}
				})
			}
		}
		_ = notifySystemdReady() // non-actionable: systemd notify is best-effort
	})
	if unregSocketCloser != nil {
		defer unregSocketCloser()
	}

	// Revocation watcher for reused identities.
	revocationDone := make(chan struct{})
	if reused {
		go watchReusedIdentityForRevocation(proxyCtx, identityKey, proxyIndex, revocationDone)
	}

	// In-process client-JWT renewal. renewalOOB was created above and
	// already wired to the connect client; the watcher shares the same
	// wrapper so its OOB calls are also intercepted for 401 audit.
	renewNow := make(chan struct{}, 1)
	go runProxyJWTWatcher(proxyCtx, proxyJWTWatcherConfig{
		IdentityKey:    identityKey,
		ClientID:       clientId,
		CurrentJWT:     byClientJwt,
		Description:    providerDescription(st.nodeName),
		DescribeFn:     func() string { return providerDescription(st.nodeName) },
		ApiURL:         st.apiUrl,
		ClientStrategy: clientStrategy,
		OOB:            renewalOOB,
		Transport:      platformTransport,
		RenewNow:       renewNow,
		ProxyIndex:     proxyIndex,
		InstanceId:     instanceId,
		RevocationDone: revocationDone,
	})

	// Note: NewLocalUserNat in v2026 no longer takes a bw parameter.
	// Per-byte bandwidth tracking via DialContextSettings wrapping is disabled
	// because it bypasses proxy routing. See DESIGN ADAPTATION in provideWithProxy.
	localUserNat := connect.NewLocalUserNat(proxyCtx, clientId.String(), localUserNatSettings)
	defer localUserNat.Close()
	// Note: NewRemoteUserNatProvider in v2026 no longer takes a bw parameter.
	remoteUserNatProvider := connect.NewRemoteUserNatProvider(connectClient, localUserNat, remoteUserNatProviderSettings)
	defer remoteUserNatProvider.Close()

	if proxySettings != nil {
		startProxyBenchmarks(proxyCtx, nil, proxySettings)
	}

	provideModes := map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public:  true,
		protocol.ProvideMode_Network: true,
	}
	connectClient.ContractManager().SetProvideModes(provideModes)

	if proxySettings != nil {
		retireMetrics := registerContractCallback(proxyIndex, connectClient)
		defer retireMetrics()
	}

	select {
	case <-proxyCtx.Done():
	}
}

// provideHandleAuthFailure logs the appropriate message after auth retries.
func provideHandleAuthFailure(st *provideState, proxyCtx context.Context, proxySettings *connect.ProxySettings, isNative, isURLSourced bool, err error) {
	if proxySettings != nil {
		if isURLSourced {
			deleteProxyCancelIfCurrent(&st.proxyCancelMu, st.proxyCancelMap, proxyCtx, proxySettings.Address)

			if errors.Is(err, errProxyURLBelowBar) {
				tlog("[proxy][init] proxy[%d] (%s) rejected by stage-1 quality gate: %v. Re-graded next fetch cycle.\n",
					getProxyIndex(proxySettings.Address), proxySettings.Address, err)
			} else if errors.Is(err, context.Canceled) {
				tlog("[proxy][init] proxy[%d] (%s) cancelled (not a give-up): %v\n",
					getProxyIndex(proxySettings.Address), proxySettings.Address, err)
			} else {
				giveUpCount := globalProxyFailureHistory.RecordGiveUp(proxySettings.Address)
				if giveUpCount >= proxyURLGiveUpEvictAfterCycles {
					if evictErr := evictProxyURLAddress(proxySettings.Address); evictErr != nil {
						fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) could not evict after %d give-ups: %v\n",
							getProxyIndex(proxySettings.Address), proxySettings.Address, giveUpCount, evictErr)
						delay := proxyURLGiveUpRetryDelay(giveUpCount)
						globalProxyFailureHistory.SetBackoffUntil(proxySettings.Address, time.Now().Add(delay))
					} else {
						fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) auth failed: Permanently removed after %d give-ups.\n",
							getProxyIndex(proxySettings.Address), proxySettings.Address, giveUpCount)
					}
				} else {
					delay := proxyURLGiveUpRetryDelay(giveUpCount)
					globalProxyFailureHistory.SetBackoffUntil(proxySettings.Address, time.Now().Add(delay))
					if reloadPath, pathErr := proxyReloadPath(); pathErr == nil {
						time.AfterFunc(delay, func() {
							if err := writeReloadTrigger(reloadPath); err != nil {
								tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
							}
						})
					}
					fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) auth failed: give-up %d/%d, will retry in %s.\n",
						getProxyIndex(proxySettings.Address), proxySettings.Address, giveUpCount, proxyURLGiveUpEvictAfterCycles, formatDuration(delay))
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) auth failed after retries: %v (proxy offline; run 'urnet-tools proxy refresh' to retry)\n",
				getProxyIndex(proxySettings.Address), proxySettings.Address, err)
		}
	} else if isNative {
		fmt.Fprintf(os.Stderr, "[proxy][init] proxy[0] (direct) auth failed after retries: %v (offline, retry on next pulse)\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[init] auth failed after retries: %v\n", err)
	}
}

// provideDirectSetup starts the native [direct] connection as proxy[0].
func provideDirectSetup(st *provideState) bool {
	noDirect := !isDirectEnabled()
	if !noDirect {
		st.wg.Add(1)
		nativeCtx, nativeCancel := context.WithCancel(st.ctx)
		st.proxyCancelMu.Lock()
		st.proxyCancelMap[directProxyKey] = nativeCancel
		st.proxyCancelMu.Unlock()
		go connect.HandleError(func() {
			defer st.wg.Done()
			defer nativeCancel()
			defer func() {
				st.proxyCancelMu.Lock()
				if cur, ok := st.proxyCancelMap[directProxyKey]; ok && reflect.ValueOf(cur).Pointer() == reflect.ValueOf(nativeCancel).Pointer() {
					delete(st.proxyCancelMap, directProxyKey)
				}
				st.proxyCancelMu.Unlock()
			}()
			gen := RegisterProxy(0, "direct")
			defer UnregisterProxySafe(0, gen)
			provideWithProxy(st, nativeCtx, nil, true, false)
		})
	} else {
		tlog("[no-direct] providing on direct/local IP is disabled; using proxy list only\n")
	}
	return !noDirect
}

// provideLauncherLoop loads proxy state, launches per-proxy goroutines,
// starts the reloader, and runs URL fetcher goroutines. Returns a no-op
// cleanup that the caller must defer (previously closed the DoH cache,
// which was removed as inert).
func provideLauncherLoop(st *provideState) func() {
	// Sentinel goroutine.
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		<-st.ctx.Done()
	}()

	// Load proxy.state.
	proxyState, stateErr := readProxyState()
	if stateErr != nil {
		tlog("[proxy] warning: could not read proxy.state: %v\n", stateErr)
		proxyState = &ProxyState{Proxies: map[string]ProxyEntry{}}
	}
	proxyState.StartedAt = provideStartTime

	highestID := -1
	for _, e := range proxyState.Proxies {
		if e.ID > highestID {
			highestID = e.ID
		}
	}
	initProxyIDCounter(highestID)

	// Select the proxy source.
	proxyFile, _ := st.opts.String("--proxy_file")
	if proxyFile == "" {
		proxyFile, _ = st.opts.String("--file")
	}
	if proxyFile == "" {
		proxyFile = os.Getenv("PROXY_FILE")
	}
	var allProxySettings []*connect.ProxySettings
	if proxyFile != "" {
		proxyFile = expandPath(proxyFile)
		settings, err := readProxySettingsFromFile(proxyFile)
		if err != nil {
			shmLogFatal(20, "[proxy] could not read proxy file: %v", err)
		}
		if len(settings) == 0 {
			shmLogFatal(21, "[proxy] proxy file %s contained no valid proxies", proxyFile)
		}
		allProxySettings = settings
		proxyState.Source = proxyFile
	} else {
		allProxySettings = readProxySettings()
		proxyState.Source = ""
	}

	// Merge URL-sourced proxies.
	primarySource := "internal"
	if proxyFile != "" {
		primarySource = "file"
	}
	proxyDesiredSet := make(map[string]*connect.ProxySettings, len(allProxySettings))
	proxySourceOf := make(map[string]string, len(allProxySettings))
	for _, s := range allProxySettings {
		proxyDesiredSet[s.Address] = s
		proxySourceOf[s.Address] = primarySource
	}
	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
	} else {
		mergeProxyURLCache(proxyDesiredSet, proxySourceOf, urlState)
	}
	allProxySettings = allProxySettings[:0]
	for _, s := range proxyDesiredSet {
		allProxySettings = append(allProxySettings, s)
	}

	if err := globalProxyEarningsStore.Load(); err != nil {
		tlog("[earn] could not read proxy earnings history: %v\n", err)
	}

	currentNetworkId := currentProviderNetworkID()
	proxySchedules, warmCount, renewableCount, coldCount := prioritizeAndScheduleProxies(allProxySettings, proxySourceOf, currentNetworkId)
	tlog("[startup] proxy prioritization: %d total (warm: %d, renewable: %d, cold: %d)\n",
		len(allProxySettings), warmCount, renewableCount, coldCount)

	if ranked, promoted, topAddr, topScore := earningsHistorySummary(allProxySettings, proxySourceOf, time.Now()); ranked == 0 {
		tlog("[startup] earnings ranking: no history yet\n")
	} else {
		tlog("[startup] earnings ranking: %d of %d proxies have earnings history, %d promoted, top earner %s at %s\n",
			ranked, len(allProxySettings), promoted, topAddr, formatBytes(uint64(topScore)))
	}

	// Start direct connection.
	provideDirectSetup(st)

	// Launch per-proxy goroutines.
	proxyState.NextID = currentProxyIDCounter()
	if err := writeProxyState(proxyState); err != nil {
		tlog("[proxy] warning: could not write proxy.state: %v\n", err)
	}

	globalProxySlowRetryState.Store(LoadProxySlowRetryState())
	setConfiguredProxyCount(len(allProxySettings))

	finishProxy := bannerPhase("Proxy load")
	if 0 < len(allProxySettings) {
		finishProxy(fmt.Sprintf("%d servers", len(allProxySettings)))

		for _, ps := range allProxySettings {
			stableID := resolveProxyID(proxyState, ps.Address)
			setProxyIndex(ps.Address, stableID)
			tagProxySourceIfUnset(proxyState, ps.Address, proxySourceOf[ps.Address])
			var user string
			var password string
			if ps.Auth != nil {
				user = ps.Auth.User
				password = ps.Auth.Password
			}
			fmt.Printf("  proxy[%d] %s (%s/%s)\n", stableID, ps.Address, obfuscateUser(user), obfuscatePassword(password))
		}

		for _, sched := range proxySchedules {
			proxySettings := sched.Settings
			proxyCtx, proxyCancel := context.WithCancel(st.ctx)
			st.proxyCancelMu.Lock()
			st.proxyCancelMap[proxySettings.Address] = proxyCancel
			proxyCtx = withProxyLaunchGen(proxyCtx, beginProxyLaunch(proxySettings.Address))
			st.proxyCancelMu.Unlock()

			stableID := getProxyIndex(proxySettings.Address)
			isURLSourced := proxySourceOf[proxySettings.Address] == "url"
			baseDelay := sched.Delay
			staggerDuration := sched.Stagger
			st.wg.Add(1)
			go connect.HandleError(func() {
				defer st.wg.Done()
				gen := RegisterProxy(stableID, proxySettings.Address)
				defer UnregisterProxySafe(stableID, gen)
				defer proxyCancel()

				if !backoffPacerWithDelay(baseDelay, staggerDuration, proxyCtx) {
					return
				}
				if !isURLSourced && proxySettings != nil && globalProxySlowRetryState.Load().WasDropped(proxySettings.Address) {
					dropAge := time.Since(globalProxySlowRetryState.Load().DropTime(proxySettings.Address))
					tlog("[proxy][slow-retry] proxy[%d] (%s) previously dropped %s ago\n",
						stableID, proxySettings.Address, formatDuration(dropAge))
				}
				proxyLaunchCount.Add(1)
				provideWithProxy(st, proxyCtx, proxySettings, false, isURLSourced)
			})
		}
	} else {
		finishProxy("no proxies configured", "⚠")
	}

	readyProfile := os.Getenv("URNETWORK_PROFILE")
	if readyProfile == "" {
		readyProfile = "default"
	}
	readyVersion := RequireVersion()
	if readyVersion == "" {
		readyVersion = "unknown"
	}
	tlog("Ready — %s | profile=%s | proxies=%d\n", readyVersion, readyProfile, len(allProxySettings))

	// Start hot-reload watcher.
	reloader := &ProxyReloader{
		cancelMap:   st.proxyCancelMap,
		cancelMapMu: &st.proxyCancelMu,
		runningAuth: make(map[string]*connect.ProxySettings),
		state:       proxyState,
		sourcePath:  proxyFile,
		parentCtx:   st.ctx,
		wg:          &st.wg,
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			provideWithProxy(st, proxyCtx, settings, isNative, isURLSourced)
		},
		drainingProxies: make(map[string]context.CancelFunc),
		directDone:      nil,
		networkID:       currentNetworkId,
	}
	// Seed runningAuth with the STARTUP launch settings BEFORE the watcher and
	// the first reload(). The startup loop above launched every proxy directly
	// (before the reloader existed), so an unseeded reload() would see no
	// recorded auth for any boot-launched proxy and rotate (cancel and
	// relaunch) all of them at boot. Seeding also has to precede the re-paste
	// case (LA7 incident: 100 proxies pasted with new creds, "added 100"
	// printed, daemon kept dialing the old user).
	reloader.seedRunningAuth(allProxySettings)
	reloader.StartWatcher(st.ctx)
	reloader.reload()

	// URL fetcher and maintenance goroutines.
	proxyURLs := resolveProxyURLs(st.opts)
	proxyURLRefresh := resolveDuration(st.opts, "--proxy_url_refresh", "PROXY_URL_REFRESH", 1*time.Hour)
	proxyURLMax := resolveInt(st.opts, "--proxy_url_max", "PROXY_URL_MAX", 500)
	cleanupScope := resolveString(st.opts, "--proxy_dead_cleanup_scope", "PROXY_DEAD_CLEANUP_SCOPE", "url")
	cleanupInterval := resolveDuration(st.opts, "--proxy_dead_cleanup_interval", "PROXY_DEAD_CLEANUP_INTERVAL", 6*time.Hour)
	selfHealEnabled := os.Getenv("URNETWORK_SELF_HEAL") == "1"
	apiProbeHost, apiProbePort := apiProbeHostPort(st.apiUrl)

	go connect.HandleError(func() {
		runProxyURLFetcher(st.ctx, proxyURLs, proxyURLRefresh, proxyURLMax, apiProbeHost, apiProbePort, selfHealEnabled)
	})
	go connect.HandleError(func() { runURLProxyReaper(st.ctx, apiProbeHost, apiProbePort) })
	go connect.HandleError(func() { runPaidProxyGrader(st.ctx, apiProbeHost, apiProbePort) })
	go connect.HandleError(func() { runProxyGradeSummary(st.ctx) })
	go connect.HandleError(func() { pruneURLProxyBlacklist(st.ctx) })
	go connect.HandleError(func() { runProxyURLCleanup(st.ctx, cleanupScope, cleanupInterval, selfHealEnabled) })
	go connect.HandleError(func() { runPressureMonitor(st.ctx, selfHealEnabled) })
	go connect.HandleError(func() { runPoolController(st.ctx, proxyURLMax, selfHealEnabled) })
	go connect.HandleError(func() { runDegradedProxyReaper(st.ctx, st.proxyCancelMap, &st.proxyCancelMu) })
	// Proxy audit: parks proven-junk paid/file proxies when proxy audit is on
	// (`urnet-tools proxy audit on`), and only logs would-park otherwise. Also
	// started in a HotSwap candidate on purpose: its memory begins at its own
	// start and its earn tracker must warm up first, so it cannot act during the
	// short overlap with the parent, and skipping it would leave a swapped node
	// without an auditor until the next full restart.
	proxyAuditEnabled := resolveProxyAuditEnabled(os.Getenv("URNETWORK_PROXY_AUDIT") == "1")
	go connect.HandleError(func() { runProxyAudit(st.ctx, st.proxyCancelMap, &st.proxyCancelMu, proxyAuditEnabled) })
	go connect.HandleError(func() { runReloadReconciler(st.ctx) })

	// Profiling.
	if profileAddr := os.Getenv("URNETWORK_PPROF"); profileAddr != "" {
		tlog("[profile] enabling diagnostics on %s\n", profileAddr)
		if err := EnableProfiling(profileAddr); err != nil {
			if st.isHotSwapCandidate && errors.Is(err, syscall.EADDRINUSE) {
				// The parent keeps the port through its stream drain; keep
				// trying so the promoted process is not left without
				// diagnostics for the rest of its life.
				tlog("[profile] %s is held by the hotswap parent; retrying until it exits\n", profileAddr)
				go connect.HandleError(func() {
					enableProfilingWithRetry(st.ctx, profileAddr, EnableProfiling, 90*time.Second, time.Second, tlog)
				})
			} else {
				tlog("[profile] failed: %v\n", err)
			}
		}
	}

	if metricsAddr := os.Getenv("URNETWORK_METRICS"); metricsAddr != "" && !st.isHotSwapCandidate {
		tlog("[metrics] enabling Prometheus /metrics on %s\n", metricsAddr)
		if ln, err := net.Listen("tcp", metricsAddr); err != nil {
			tlog("[metrics] listener failed: %v\n", err)
		} else {
			serveMetrics(ln)
		}
	}

	return func() {}
}

// provideStatusServer starts the optional status HTTP server.
func provideStatusServer(st *provideState) {
	port, _ := st.opts.Int("--port")
	if 0 < port {
		tlog("[startup] status server listening on :%d\n", port)
		statusServer := &http.Server{
			Addr:              fmt.Sprintf(":%d", port),
			Handler:           &Status{},
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			for {
				err := statusServer.ListenAndServe()
				if errors.Is(err, http.ErrServerClosed) {
					return
				}
				if err != nil {
					tlog("[status] error: %v — retrying in 30s\n", err)
				}
				select {
				case <-time.After(30 * time.Second):
					continue
				case <-st.ctx.Done():
					return
				}
			}
		}()
	} else {
		tlog("[startup] status server (no port)\n")
	}

	printReadyUnlessCondensed()
}

// closeAllCaches flushes caches before exit.
func closeAllCaches(st *provideState) {
	FlushPersistentErrors()
	forceAuditPersist()
	metricsMu.Lock()
	srv := metricsServer
	metricsMu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}
	markCleanShutdown()
	if st.cleanupControlSocket != nil {
		st.cleanupControlSocket()
	}
}

// provideStartTime records when provide() began; used for uptime display
// and warmup pacing.
var provideStartTime time.Time

// proxyLaunchCount tracks how many proxy goroutines have passed the stagger
// delay and entered provideWithProxy. Used by paceMonitor for progress logging.
var proxyLaunchCount atomic.Int64

// EnableProfiling starts a pprof HTTP listener on addr so that the daemon
// exposes /debug/pprof/heap, /debug/pprof/goroutine, etc. for live
// diagnostics. Previously a no-op stub returning nil; wired to Go's
// standard net/http/pprof since v2026 connect does not provide its own
// profiling endpoint.
func EnableProfiling(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("pprof listen on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	// Register all pprof handlers on our private mux (avoids polluting
	// http.DefaultServeMux which could conflict with metrics or other
	// services).
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// Named profile endpoints: pprof.Handler returns an http.Handler;
	// we wrap it into HandleFunc's signature.
	for _, name := range []string{"allocs", "block", "goroutine", "mutex", "heap", "threadcreate"} {
		name := name // capture loop var
		mux.HandleFunc("/debug/pprof/"+name, func(w http.ResponseWriter, r *http.Request) {
			pprof.Handler(name).ServeHTTP(w, r)
		})
	}
	tlog("[profile] pprof listening on %s\n", addr)
	go http.Serve(ln, mux) //nolint:errcheck // best-effort diagnostic server
	return nil
}

// paceMonitor logs real-time warmup progress every 30s and flips
// proxyWarmupDone once the initial file-proxy ramp is judged complete.
// Ported from fork main.go's paceMonitor: connect.ProxyHealthSnapshot()/
// connect.ProxyHealthCount() became the package-local ProxyHealthSnapshot()/
// ProxyHealthCount() (proxy_health.go), which are fully ported and real.
//
// Without this, proxyWarmupDone.Store(true) is never called: URL-sourced
// proxies (proxy_url_source.go:1118) and hot-reloaded proxies
// (proxy_reload.go:648) both gate on proxyWarmupDone and would wait forever.
func paceMonitor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		up, _, _, _, connecting := ProxyHealthSnapshot()
		total := ProxyHealthCount()
		if total < 5 {
			tlog("🔥 [pace] ✓ warmup: %d up, %d total (< 5) — done\n", up, total)
			proxyWarmupDone.Store(true)
			signalProxyReloadAfterWarmup()
			return
		}
		pct := float64(up) * 100 / float64(total)
		connectingN := len(connecting)
		elapsed := time.Since(provideStartTime)
		if elapsed > 60*time.Minute {
			tlog("🔥 [pace] warmup: %d/%d up (%.0f%%), %d connecting — forced done after 60m\n",
				up, total, pct, connectingN)
			proxyWarmupDone.Store(true)
			signalProxyReloadAfterWarmup()
			return
		}
		if pct < 50 && connectingN > 10 {
			tlog("🔥 [pace] ⚠ warmup: %d/%d up (%.0f%%), %d connecting, %d done\n",
				up, total, pct, connectingN, total-up-connectingN)
		} else if pct > 90 && connectingN < 5 {
			tlog("🔥 [pace] ✓ warmup: %d/%d up (%.0f%%), %d connecting — done\n",
				up, total, pct, connectingN)
			proxyWarmupDone.Store(true)
			signalProxyReloadAfterWarmup()
			return
		} else {
			tlog("🔥 [pace] warmup: %d/%d up (%.0f%%), %d connecting\n",
				up, total, pct, connectingN)
		}
	}
}

// signalProxyReloadAfterWarmup nudges the URL-sourced proxy loop to pick up
// now that file-proxy warmup has completed and it is no longer held back by
// proxyWarmupDone.
func signalProxyReloadAfterWarmup() {
	if reloadPath, err := proxyReloadPath(); err == nil {
		if err := writeReloadTrigger(reloadPath); err != nil {
			tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
		}
	}
}
