package provider

import (
	"context"
	"errors"
	"fmt"
	"github.com/urfoundation/sn/internal/connectx"
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

// proxyIndexByKey maps proxy identity keys (ProxySettings.Key()) to their
// stable integer IDs. Needed because the new connect.ProxySettings doesn't
// carry an Index field (the old fork added it). Populated in
// provideLauncherLoop and read in provideWithProxy.
var proxyIndexByKey sync.Map

func setProxyIndex(key string, idx int) {
	proxyIndexByKey.Store(key, idx)
}

// deleteProxyIndex removes a proxy identity key from the index map.
// Called from UnregisterProxy to prevent unbounded growth.
func deleteProxyIndex(key string) {
	proxyIndexByKey.Delete(key)
}

// getProxyIndex returns the stable integer ID for a proxy identity key,
// or -1 if the key was never registered. Callers must check for
// -1 to avoid misattributing health metrics to the direct proxy (index 0).
func getProxyIndex(key string) int {
	if v, ok := proxyIndexByKey.Load(key); ok {
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
	// Wire the Prometheus pool snapshot to this process's own health registry
	// before anything can serve a scrape. connectx owns no health state, so
	// without this the /metrics handler omits the proxy-pool families entirely
	// (which is the correct safe default, but it would mean the pool is never
	// reported). Typed exactly as sn's ProxyHealthSnapshot so it satisfies the
	// seam without a wrapper.

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
		tlog("♻️⚡ [hotswap] Candidate PID %d promoted to live provider (version=%s)\n", os.Getpid(), RequireVersion())
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

	// The baseline recorder. Started here, beside the other long-lived
	// samplers and tied to the SAME ctx, so it stops when the provider stops:
	// a recorder that outlives its process would be a second writer on the file
	// after a hot swap promoted the candidate. baselineStart writes the start
	// mark before launching the goroutine, because a sample with no start beside
	// it cannot be told apart from one the operator never had.
	baselineStart(st.ctx, currentVersionForBaseline(), previousVersionForBaseline())

	go connect.HandleError(func() { paceMonitor(st.ctx) })
}

// provideWithProxy runs a single proxy's transport lifecycle.
func provideWithProxy(st *provideState, proxyCtx context.Context, proxySettings *connect.ProxySettings, isNative bool, isURLSourced bool) {
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ProxySettings = proxySettings

	// Compute proxy identity early so we can wire bandwidth tracking
	// before the client strategy (and its DialContext) is created.
	identityKey := jwtStoreKey(proxySettings)
	proxyIndex := 0
	if proxySettings != nil {
		proxyIndex = getProxyIndex(proxySettings.Key())
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
	// First proxy goroutine: the tier/profile limits are in place now, so this
	// is the first point where the soft memory limit in force is the one this
	// process will actually run under. Read at the old call site (before the
	// first launch) it saw no limit at all on every auto, eco, turbo and
	// default node, because the tier code sets GOMEMLIMIT here.
	resourceConfigWarningsOnce()
	// Wrap the relay-egress ConnectSettings with byte counting. This is a
	// separate copy from clientStrategySettings.ConnectSettings on purpose —
	// wrapping in place would also instrument the provider's own
	// control-plane traffic to the platform API, double-counting it as
	// billable proxy bandwidth.
	relayConnectSettings := bandwidth.WrapConnectSettings(clientStrategySettings.ConnectSettings, proxyBandwidth, identityKey)
	localUserNatSettings.TcpBufferSettings.ConnectSettings = relayConnectSettings
	localUserNatSettings.UdpBufferSettings.ConnectSettings = relayConnectSettings
	// The provider's own connections to the platform (API, auth, the H1 tunnel)
	// count into the total only. This is a copy made after the relay copy above
	// so relay bytes are not counted twice, and a direct identity keeps the
	// engine's own dial. See bandwidth.WrapConnectSettingsTotal.
	clientStrategySettings.ConnectSettings = bandwidth.WrapConnectSettingsTotal(clientStrategySettings.ConnectSettings, proxyBandwidth, identityKey)
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
		if proxySettings != nil && !globalProvenProxies.HasSucceeded(proxySettings.Key()) {
			maxAuthFailures = unprovenMaxAuthFailures
		}
		authFailures := 0
		// Per-attempt measurements for this proxy's retry ladder: how long the
		// attempt waited for an admission slot, how long the attempt itself
		// ran, its raw error, and whether it was cut short by the connect
		// deadline (i.e. slow rather than broken).
		var admitWait time.Duration
		var attemptDuration time.Duration
		var attemptErr error
		var cutShortByDeadline bool
		// Slow attempts (cut short by the deadline) and genuine failures are
		// counted separately: a slow-but-working proxy must not have its attempts
		// advance the give-up budget that ends in eviction. Both counters share
		// the same ceiling, so the ladder still terminates for a proxy that only
		// ever times out.
		cutShortAttempts := 0
		// slowRetryCycles counts consecutive slow (deadline-cut) give-up
		// cycles for the non-URL retry ramp. It is deliberately NOT
		// persisted: the persisted-state guard routes back through the
		// genuine-failure path, which clears it and continues on the
		// authFailures-based delay.
		slowRetryCycles := 0
		// genuineRetryCycles counts consecutive genuine (non-slow) give-up
		// cycles for the non-URL retry ramp's daily gate. authFailures is
		// pinned near its ceiling after each cycle (see below) rather than
		// growing without bound or resetting to 0, so it can no longer
		// drive the ramp math itself; this dedicated counter does.
		genuineRetryCycles := 0

		if proxySettings != nil && !isURLSourced {
			if globalProxySlowRetryState.Load().WasDropped(proxySettings.Key()) || globalProxySlowRetryState.Load().TimeUntilNextAttempt(proxySettings.Key()) > 0 {
				authFailures = maxAuthFailures
			}
		}
		for {
			var err error
			var byClientJwt string
			var clientId connect.Id
			var reused bool

			// Per-attempt state must be fresh for this iteration: the previous
			// attempt's measurements and cut-short classification describe a
			// DIFFERENT error and must not leak into its accounting.
			admitWait = 0
			attemptDuration = 0
			attemptErr = nil
			cutShortByDeadline = false

			if proxySettings != nil && isURLSourced && !urlProxyPassesAdmission(proxyCtx, proxySettings.Address) {
				cfg := resolveProxyTableProbeConfig()
				if score, ok := cachedProxyURLScore(proxySettings.Address); ok && cfg.Enabled && score < cfg.PassBar {
					return "", connect.Id{}, false, fmt.Errorf("%w: %s (score %.2f)", errProxyURLBelowBar, proxySettings.Address, score)
				}
				err = fmt.Errorf("proxy unreachable: %s", proxySettings.Address)
			} else {
				err = func() error {
					// The direct connection (proxySettings == nil) is the
					// provider's own identity, not a paid/free proxy: it must
					// never queue behind slow or dead proxies for a shared
					// 3-slot semaphore, even if it enters slow-retry mode
					// itself. Also acquire the semaphore BEFORE the admission
					// gate (not after): otherwise a slow-retry proxy holds an
					// admission slot while it waits on/holds the semaphore,
					// starving admission for healthy proxies behind it.
					useSemaphore := usesSlowRetrySemaphore(proxySettings != nil, isURLSourced, authFailures, maxAuthFailures, slowRetryCycles, genuineRetryCycles)
					if useSemaphore {
						select {
						case slowRetrySemaphore <- struct{}{}:
							defer func() { <-slowRetrySemaphore }()
						case <-proxyCtx.Done():
							return proxyCtx.Err()
						}
					}
					admitFailureCount := authFailures
					if proxySettings != nil {
						admitFailureCount = globalProxyFailureHistory.FailureCount(proxySettings.Key())
					}
					admitStart := time.Now()
					release, waitErr := globalProxyAdmissionGate.Admit(proxyCtx, admitFailureCount)
					admitWait = time.Since(admitStart)
					if waitErr != nil {
						return waitErr
					}
					defer release()

					identityKey := jwtStoreKey(proxySettings)
					attemptStart := time.Now()
					byClientJwt, clientId, reused, err = provideAuth(proxyCtx, clientStrategy, st.apiUrl, st.opts, st.nodeName, identityKey)
					attemptDuration = time.Since(attemptStart)
					attemptErr = err
					return err
				}()
				// Decide, from the MEASURED duration, whether the connect
				// deadline cut this dial short. Such an attempt is slow, not
				// broken: it must not read as a proxy failure, and it must not
				// drag the shared auth rate down (that turns proxy latency into
				// a failure spiral — the rate drops, every remaining proxy waits
				// longer, and those wait past the deadline in turn).
				cutShortByDeadline = authTimeoutCutShortThreshold <= attemptDuration && isTimeoutFamilyError(attemptErr)
				if proxySettings != nil {
					if err == nil {
						globalProvenProxies.MarkSucceeded(proxySettings.Key())
						globalProxyFailureHistory.Reset(proxySettings.Key())
						globalProxySlowRetryState.Load().ClearDropped(proxySettings.Key())
					}
					if !cutShortByDeadline {
						globalAuthRateLimiter.ReportResultForProxy(err, globalProvenProxies.HasSucceeded(proxySettings.Key()))
					}
				} else {
					// Direct (non-proxy) path: there is no proxy whose
					// latency could fake an API overload, so a sustained
					// timeout-family error IS an overload signal — report
					// it to the shared limiter even when the attempt was
					// cut short by the connect deadline.
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

			// A deadline-cut attempt is slow, not broken: it must not move the
			// give-up budget that ends in eviction or the 14-day drop, but
			// it still counts toward the ladder's own ceiling so the loop
			// terminates for a proxy that only ever times out.
			if cutShortByDeadline {
				cutShortAttempts++
			} else {
				authFailures++
			}
			if proxySettings != nil {
				// Only genuine failures count against the proxy's history. A
				// dial cut off by the connect deadline is slow, not broken;
				// recording it would demote the proxy in the admission lottery
				// and start a spiral of longer waits and more timeouts.
				if !cutShortByDeadline {
					globalProxyFailureHistory.RecordFailure(proxySettings.Key())
				}
				RecordProxyAuthFailure(proxyIndex, err)
			}
			if authFailures >= maxAuthFailures || cutShortAttempts >= maxAuthFailures {
				// The ladder ended either on genuine failures or on slow
				// (deadline-cut) attempts. When the slow budget filled first,
				// the proxy is slow, not broken: it must not enter the give-up
				// accounting that ends in eviction (URL) or the 14-day drop
				// (non-URL).
				gaveUpOnSlow := cutShortByDeadline && cutShortAttempts >= maxAuthFailures && authFailures < maxAuthFailures
				ladderAttempts := authFailures + cutShortAttempts
				cause := classifyAuthFailureCause(err, proxySettings != nil)
				// One diagnostic line per give-up, with the raw error and the
				// measured split between waiting for an admission slot and the
				// attempt itself, so latency can be told apart from a refusal.
				if proxySettings != nil {
					tlog("[proxy][auth] proxy[%d] (%s) attempts=%d admit_wait=%s attempt=%s cut_short=%t err=%v\n",
						getProxyIndex(proxySettings.Key()), proxySettings.Address, ladderAttempts,
						formatSeconds(admitWait), formatSeconds(attemptDuration), cutShortByDeadline, attemptErr)
				} else {
					tlog("[proxy][auth] direct attempts=%d admit_wait=%s attempt=%s cut_short=%t err=%v\n",
						ladderAttempts, formatSeconds(admitWait), formatSeconds(attemptDuration), cutShortByDeadline, attemptErr)
				}
				if isURLSourced {
					// Slow-shielding is a reward for a proven track record,
					// not a blanket amnesty for anything that hangs until
					// the deadline: an entry that has NEVER once succeeded
					// gets no signal from "slow" beyond "still unproven,"
					// and shielding it would retry forever with backoff
					// instead of ever reaching the normal give-up/eviction
					// path that a never-worked entry should take.
					if gaveUpOnSlow && globalProvenProxies.HasSucceeded(proxySettings.Key()) {
						// Slow, not broken: do not enter give-up accounting.
						// The outer handler recognizes the sentinel and backs
						// off + requeues without RecordGiveUp, so a
						// slow-but-working list entry is never evicted.
						return "", connect.Id{}, false, fmt.Errorf("%w: %s", errProxyURLSlowCutShort, proxySettings.Address)
					}
					return "", connect.Id{}, false, fmt.Errorf("authentication failed after %d attempts — %s: %w", maxAuthFailures, cause, err)
				}
				if gaveUpOnSlow {
					// Non-URL slow give-up: bounded slow retry WITHOUT the
					// 14-day drop clock. The delay ramps 5m/10m/15m then
					// daily, so a slow-but-working paid/direct proxy keeps
					// trying on that schedule instead of being dropped from
					// the active pool for being slow. Never
					// RecordSlowRetryStart/ShouldDrop/MarkDropped here.
					// The ramp advances on slowRetryCycles, NOT on
					// authFailures. cutShortAttempts is set to one below its
					// ceiling (not zeroed) so exactly ONE further attempt —
					// of either kind — re-enters this block, correctly
					// classified by its own outcome: one attempt per ramp
					// step, matching the pre-existing cadence, instead of
					// replaying a full ladder of up to maxAuthFailures
					// attempts every cycle. authFailures is left untouched
					// (it was already < maxAuthFailures to have reached this
					// branch), so a genuine failure on that one attempt
					// still needs its own share of the ceiling before it can
					// end in a genuine give-up — a stale ceiling must not
					// make it read as slow.
					slowRetryCycles++
					slowDelay := proxyAuthSlowRetryDelay(slowRetryCycles)
					if proxySettings != nil {
						tlog("[proxy][slow-retry] proxy[%d] (%s) auth slow after %d attempts (%s); retrying in %s (not counted as a drop)\n",
							getProxyIndex(proxySettings.Key()), proxySettings.Address, ladderAttempts, cause, formatDuration(slowDelay))
					} else if isNative {
						tlog("[proxy][slow-retry] proxy[0] (direct) auth slow after %d attempts (%s); retrying in %s (not counted as a drop)\n",
							ladderAttempts, cause, formatDuration(slowDelay))
					}
					cutShortAttempts = maxAuthFailures - 1
					select {
					case <-proxyCtx.Done():
						return "", connect.Id{}, false, proxyCtx.Err()
					case <-time.After(slowDelay):
						continue
					}
				}
				// The genuine-failure path owns the persisted ramp: clear
				// the local slow-cycle counter so the semaphore condition
				// and the delay both fall back to the authFailures-based
				// accounting.
				slowRetryCycles = 0
				// genuineRetryCycles drives the ramp/daily-gate math below
				// instead of authFailures: authFailures is pinned one below
				// its ceiling (not left at/above it) so exactly ONE further
				// attempt — of either kind — re-enters this give-up branch,
				// correctly classified by its own outcome. Leaving
				// authFailures >= maxAuthFailures permanently (the pre-fix
				// behavior) made every later attempt's gaveUpOnSlow check
				// read authFailures < maxAuthFailures as false forever, so a
				// slow-but-working proxy could never be reclassified as slow
				// again after one genuine give-up — it kept retrying every
				// ~5 minutes instead of ramping to the daily cadence, while
				// the 14-day drop clock still advanced underneath it. This
				// applies to the direct connection too (proxySettings ==
				// nil skips only the PERSISTED bookkeeping below, which is
				// keyed by proxy address).
				genuineRetryCycles++
				authFailures = maxAuthFailures - 1
				cutShortAttempts = maxAuthFailures - 1
				if proxySettings != nil {
					startedAt := globalProxySlowRetryState.Load().RecordSlowRetryStart(proxySettings.Key())
					if globalProxySlowRetryState.Load().ShouldDrop(proxySettings.Key()) {
						globalProxySlowRetryState.Load().MarkDropped(proxySettings.Key())
						dropAge := time.Since(startedAt)
						tlog("[proxy][slow-retry] proxy[%d] (%s) dropped after %s of continuous failure (%d give-up cycles)\n",
							getProxyIndex(proxySettings.Key()), proxySettings.Address, formatDuration(dropAge), genuineRetryCycles)
						var cancel context.CancelFunc
						st.proxyCancelMu.Lock()
						if proxyOwnsLaunch(proxyCtx, proxySettings.Key()) {
							cancel = st.proxyCancelMap[proxySettings.Key()]
							delete(st.proxyCancelMap, proxySettings.Key())
						}
						st.proxyCancelMu.Unlock()
						if cancel != nil {
							cancel()
						}
						return "", connect.Id{}, false, fmt.Errorf("proxy dropped after %s of continuous failure — %s", formatDuration(dropAge), cause)
					}
					if genuineRetryCycles > slowRetryRampAttempts && !globalProxySlowRetryState.Load().RecordSlowRetryAttempt(proxySettings.Key()) {
						waitTime := globalProxySlowRetryState.Load().TimeUntilNextAttempt(proxySettings.Key())
						if waitTime <= 0 {
							waitTime = 24 * time.Hour
							tlog("[proxy][slow-retry] proxy[%d] (%s) waitTime was non-positive, clamping to %s\n",
								getProxyIndex(proxySettings.Key()), proxySettings.Address, formatDuration(waitTime))
						}
						tlog("[proxy][slow-retry] proxy[%d] (%s) auth still failing after %d cycles (%s); next check in %s\n",
							getProxyIndex(proxySettings.Key()), proxySettings.Address, genuineRetryCycles, cause, formatDuration(waitTime))
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
				slowDelay := proxyAuthSlowRetryDelay(genuineRetryCycles)
				if proxySettings != nil {
					tlog("[proxy][init] proxy[%d] (%s) auth still failing after %d cycles (%s); retrying in %s\n",
						getProxyIndex(proxySettings.Key()), proxySettings.Address, genuineRetryCycles, cause, formatDuration(slowDelay))
				} else if isNative {
					tlog("[proxy][init] proxy[0] (direct) auth still failing after %d cycles (%s); retrying in %s\n",
						genuineRetryCycles, cause, formatDuration(slowDelay))
				} else {
					tlog("[init] auth still failing after %d cycles (%s); retrying in %s\n",
						genuineRetryCycles, cause, formatDuration(slowDelay))
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
					getProxyIndex(proxySettings.Key()), proxySettings.Address, authFailures, maxAuthFailures, err, float64(retryDelay/time.Millisecond)/1000.0)
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

	// The smart dialer ranks transports it has measured, and the first-choice
	// transport always wins where it works, so the others are never dialed and
	// never measured. Probe them in the background (a no-op while the smart
	// dialer is off). Started only now, after this proxy authenticated: a proxy
	// that never gets through auth, or sits in slow retry, must not probe, and
	// the scheduler keeps the probes behind the auth admission gate besides.
	// See startSmartDialerProbes.
	probeTag, probeAddr, probeViaSocks := "direct", "direct", false
	if proxySettings != nil {
		probeTag, probeAddr, probeViaSocks = "proxy", proxySettings.Address, true
	}
	startSmartDialerProbes(proxyCtx, clientStrategy, st.apiUrl, probeTag, proxyIndex, probeAddr, probeViaSocks)

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
	// The platform transport's H3 (QUIC) modes open a UDP socket, which left
	// alone is a socket on the host: a proxied identity that wins the H3 race
	// would reach the platform from the host's address, not its proxy's. The
	// factory relays a proxied identity's QUIC through its proxy (and fails
	// closed to a TCP mode if the proxy cannot), and counts the bytes when
	// there is a tracker. A direct identity with nothing to count keeps the
	// engine's default socket. See newH3PacketConnFactory.
	//
	// Health follows the transport's real connection state rather than its
	// construction: see proxy_transport_health.go. Its logger observes the
	// engine's per-attempt connect failures.
	transportHealth := newProxyTransportHealth(proxyCtx, proxyIndex)
	platformSettings := connect.DefaultPlatformTransportSettings()
	platformSettings.Log = transportHealth.logger(connect.DefaultLogger())
	// The DATAGRAM offer on an H3 connection is an operator gate, not
	// connect's own default of true. snH3Eligible is the single rollout
	// predicate; see h3_datagram.go. The value is read at each H3 dial, so
	// applying it here covers the first dial and the control socket updates the
	// running transport live.
	//
	// The eligibility read and the running-entry registration are ONE atomic
	// step. A concurrent control update or reload re-resolve runs
	// reapplyH3ModeLive, which skips an untracked key on the assumption that the
	// key reads the mode when it builds its transport. Reading the eligibility
	// before registering would let that re-apply observe this identity
	// untracked and skip it, after which the launch would register the stale
	// value and wire stale ModePreferences, leaving the identity on the wrong
	// side of the cap until the next control update. registerH3RunningEligible
	// holds h3RunningMu across both, so the re-apply either sees the
	// registration (and reconnects the identity if the mode moved) or runs
	// before it, in which case the read here sees the newer mode.
	h3IdentityKey, h3Eligible, h3Launch := registerH3RunningEligible(proxySettings, isNative)
	defer unregisterH3RunningIfCurrent(h3IdentityKey, h3Launch)
	// Which identities run H3 is the `h3` control key's decision; an excluded
	// identity is pinned to H1 only so the engine never dials H3 at all.
	applyH3ModeToSettings(platformSettings, h3Eligible)
	if h3Eligible {
		applyH3DatagramOfferToSettings(platformSettings)
		// One collector across every transport, so DATAGRAM counters are a
		// process total rather than whichever transport is sampled last. See
		// h3_datagram.go.
		platformSettings.H3DatagramStats = h3DatagramProcessStats
	}
	if factory := newH3PacketConnFactory(proxySettings, proxyBandwidth, identityKey); factory != nil {
		platformSettings.H3PacketConnFactory = factory
	}
	platformTransport := connect.NewPlatformTransport(proxyCtx, clientStrategy, connectClient.RouteManager(), st.connectUrl, auth, platformSettings)
	unregCloser := RegisterCoordinatorCloser(func() {
		platformTransport.Close()
	})
	defer unregCloser()
	if h3Eligible {
		applyH3DatagramSendToSettings(platformSettings)
		registerH3DatagramTarget(platformSettings, platformTransport)
		defer unregisterH3DatagramTarget(platformTransport)
	}

	proxyBecameLive()
	defer proxyWentDown()
	defer markProxyDown(proxyIndex)
	stopTransportHealth := transportHealth.start(platformTransport)
	defer stopTransportHealth()

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
			// The parent persists again at the end of its drain, from a ring
			// without our entries. Re-merge and persist once that is over so
			// a crash soon after takeover cannot lose our start entry.
			go connect.HandleError(func() {
				reconcileAuditRingAfterHandoff(st.ctx, HotSwapDrainTimeout+auditReconcileGrace)
			})

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

	// Note: NewLocalUserNat in v2026 no longer takes a bw parameter. Relay-egress
	// sockets are still counted into the TOTAL by WrapConnectSettings (see
	// above); billable comes from the remote provider's relay stats below.
	localUserNat := connect.NewLocalUserNat(proxyCtx, clientId.String(), localUserNatSettings)
	defer localUserNat.Close()
	// Note: NewRemoteUserNatProvider in v2026 no longer takes a bw parameter.
	remoteUserNatProvider := connect.NewRemoteUserNatProvider(connectClient, localUserNat, remoteUserNatProviderSettings)
	defer remoteUserNatProvider.Close()
	// Billable bytes come from the provider's own relay accounting, not from the
	// egress socket wrapper: see bandwidth.ProxyBandwidth.RelayStatsObserver.
	// The engine fires this about once a second and has no final flush at
	// shutdown, so the last epoch is read directly after unsubscribing, before
	// the provider closes (defers run in reverse, and Close was deferred above).
	if proxyBandwidth != nil {
		observeRelay := proxyBandwidth.RelayStatsObserver()
		unsubscribeRelayStats, err := remoteUserNatProvider.TryAddPacketStatsCallback(func(stats *connect.PacketStats) {
			observeRelay(uint64(max(stats.RemoteIngressByteCount, 0)), uint64(max(stats.RemoteEgressByteCount, 0)))
		})
		if err != nil {
			// without the callback billable stays zero for this proxy; say so
			tlog("[proxy][billable] proxy[%d] relay stats unavailable, billable bytes will not be counted: %v\n", proxyIndex, err)
		} else {
			defer func() {
				unsubscribeRelayStats()
				if stats := remoteUserNatProvider.PacketStats(); stats != nil {
					observeRelay(uint64(max(stats.RemoteIngressByteCount, 0)), uint64(max(stats.RemoteEgressByteCount, 0)))
				}
			}()
		}
	}

	// The platform's own per-contract accounting, kept beside billable as a
	// cross-check (urnet_contract_used_bytes_total). A duplicate or stale event
	// is discarded by sequence, and a closed contract is forgotten.
	if proxyBandwidth != nil {
		unsubscribeContractStats := connectClient.ContractManager().AddContractStatsCallback(proxyBandwidth.ContractStatsObserver())
		defer unsubscribeContractStats()
	}

	if proxySettings != nil {
		startProxyBenchmarks(proxyCtx, proxyBandwidth, proxySettings)
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
			deleteProxyCancelIfCurrent(&st.proxyCancelMu, st.proxyCancelMap, proxyCtx, proxySettings.Key())

			if errors.Is(err, errProxyURLBelowBar) {
				tlog("[proxy][init] proxy[%d] (%s) rejected by stage-1 quality gate: %v. Re-graded next fetch cycle.\n",
					getProxyIndex(proxySettings.Key()), proxySettings.Address, err)
			} else if errors.Is(err, context.Canceled) || proxyCtx.Err() != nil {
				// Context cancellation (trim, reaper, reload, drain) is not an
				// auth failure even when the ladder surfaced a different error
				// on the way out. Recording a give-up would eventually evict a
				// healthy proxy after enough operational cycles.
				tlog("[proxy][init] proxy[%d] (%s) cancelled (not a give-up): %v\n",
					getProxyIndex(proxySettings.Key()), proxySettings.Address, err)
			} else if errors.Is(err, errProxyURLSlowCutShort) {
				// Slow, not broken: the ladder ended on deadline-cut attempts,
				// not genuine failures. Back off and requeue WITHOUT give-up
				// accounting, so a slow-but-working list entry is never
				// permanently evicted by latency.
				tlog("[proxy][init] proxy[%d] (%s) auth slow (deadline-cut); not a give-up, requeue with backoff: %v\n",
					getProxyIndex(proxySettings.Key()), proxySettings.Address, err)
				delay := proxyURLGiveUpRetryDelay(proxyURLGiveUpEvictAfterCycles - 1)
				globalProxyFailureHistory.SetBackoffUntil(proxySettings.Key(), time.Now().Add(delay))
				if reloadPath, pathErr := proxyReloadPath(); pathErr == nil {
					time.AfterFunc(delay, func() {
						if err := writeReloadTrigger(reloadPath); err != nil {
							tlog("[proxy] warn: failed to signal proxy reload after slow-auth backoff (write .reload): %v\n", err)
						}
					})
				}
			} else {
				giveUpCount := globalProxyFailureHistory.RecordGiveUp(proxySettings.Key())
				if giveUpCount >= proxyURLGiveUpEvictAfterCycles {
					if evictErr := evictProxyURLAddress(proxySettings.Address); evictErr != nil {
						fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) could not evict after %d give-ups: %v\n",
							getProxyIndex(proxySettings.Key()), proxySettings.Address, giveUpCount, evictErr)
						delay := proxyURLGiveUpRetryDelay(giveUpCount)
						globalProxyFailureHistory.SetBackoffUntil(proxySettings.Key(), time.Now().Add(delay))
					} else {
						fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) auth failed: Permanently removed after %d give-ups.\n",
							getProxyIndex(proxySettings.Key()), proxySettings.Address, giveUpCount)
					}
				} else {
					delay := proxyURLGiveUpRetryDelay(giveUpCount)
					globalProxyFailureHistory.SetBackoffUntil(proxySettings.Key(), time.Now().Add(delay))
					if reloadPath, pathErr := proxyReloadPath(); pathErr == nil {
						time.AfterFunc(delay, func() {
							if err := writeReloadTrigger(reloadPath); err != nil {
								tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
							}
						})
					}
					fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) auth failed: give-up %d/%d, will retry in %s.\n",
						getProxyIndex(proxySettings.Key()), proxySettings.Address, giveUpCount, proxyURLGiveUpEvictAfterCycles, formatDuration(delay))
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) auth failed after retries: %v (proxy offline; run 'urnet-tools proxy refresh' to retry)\n",
				getProxyIndex(proxySettings.Key()), proxySettings.Address, err)
		}
	} else if isNative {
		fmt.Fprintf(os.Stderr, "[proxy][init] proxy[0] (direct) auth failed after retries: %v (offline, retry on next pulse)\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "[init] auth failed after retries: %v\n", err)
	}
}

// provideDirectSetup starts the native [direct] connection as proxy[0]. It
// returns a channel closed when the direct goroutine exits, or nil when direct
// is disabled.
func provideDirectSetup(st *provideState) chan struct{} {
	return startDirectTransport(st, func(nativeCtx context.Context) {
		provideWithProxy(st, nativeCtx, nil, true, false)
	})
}

// startDirectTransport runs the direct transport in a goroutine registered as
// proxy[0] and under directProxyKey in the cancel map. The returned channel is
// published to the reloader so a `direct off` before any reload has
// hot-toggled direct still waits for this startup goroutine (it would
// otherwise unregister proxy[0] out from under a still-running transport).
func startDirectTransport(st *provideState, run func(nativeCtx context.Context)) chan struct{} {
	if !isDirectEnabled() {
		tlog("[no-direct] providing on direct/local IP is disabled; using proxy list only\n")
		return nil
	}
	st.wg.Add(1)
	nativeCtx, nativeCancel := context.WithCancel(st.ctx)
	st.proxyCancelMu.Lock()
	st.proxyCancelMap[directProxyKey] = nativeCancel
	st.proxyCancelMu.Unlock()
	done := make(chan struct{})
	go connect.HandleError(func() {
		defer close(done) // first defer = runs last (LIFO)
		defer st.wg.Done()
		defer nativeCancel()
		defer func() {
			st.proxyCancelMu.Lock()
			if cur, ok := st.proxyCancelMap[directProxyKey]; ok && reflect.ValueOf(cur).Pointer() == reflect.ValueOf(nativeCancel).Pointer() {
				delete(st.proxyCancelMap, directProxyKey)
			}
			st.proxyCancelMu.Unlock()
		}()
		gen := RegisterProxy(0, "direct", "direct")
		defer UnregisterProxySafe(0, gen)
		run(nativeCtx)
	})
	return done
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
		proxyDesiredSet[s.Key()] = s
		proxySourceOf[s.Key()] = primarySource
	}
	urlCacheLoaded := true
	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
		urlCacheLoaded = false
	} else {
		mergeProxyURLCache(proxyDesiredSet, proxySourceOf, urlState)
	}
	allProxySettings = allProxySettings[:0]
	for _, s := range proxyDesiredSet {
		allProxySettings = append(allProxySettings, s)
	}

	// Publish the full desired set (internal or file source, plus URL-sourced)
	// so a cap ranks over every proxy the box runs, and re-resolve the current
	// mode now that the set is known. A persisted `h3 = N` replays before this
	// point, when only the internal config was readable, so without the
	// re-resolve a file- or URL-fed box would cap to zero proxies.
	//
	// Gated on urlCacheLoaded, matching the reload path: an unreadable
	// proxy_url.json drops every URL-sourced proxy from allProxySettings, so
	// publishing here would install a partial candidate set. A later successful
	// reload publishes the complete set.
	if urlCacheLoaded {
		publishH3ProxyCandidates(allProxySettings)
		reResolveActiveH3Cap()
	}

	// Migrate legacy bare-address state entries before anything reads proxyState
	// by key, and adopt saved client logins onto identity keys before the
	// warmth evaluation below consults the client-JWT store.
	adoptLegacyProxyState(proxyState, allProxySettings)
	if store := loadGlobalClientJWTStore(); store != nil {
		store.AdoptLegacy(allProxySettings)
		// An unreadable proxy_url.json leaves every URL-sourced proxy out of
		// allProxySettings; pruning then would delete their saved logins.
		store.PruneUndesired(allProxySettings, urlCacheLoaded)
	}

	if err := globalProxyEarningsStore.Load(); err != nil {
		tlog("[earn] could not read proxy earnings history: %v\n", err)
	}
	// Adopt any legacy bare-address earnings entries to their identity keys
	// right after load, and before the launch scheduler reads scores.
	globalProxyEarningsStore.adoptLegacy(allProxySettings)

	currentNetworkId := currentProviderNetworkID()
	// Decide the startup cap BEFORE scheduling anything, so the OOM-aware cap
	// set since the last start applies to this very start (enforced only with
	// URNETWORK_OOM_CAP=on; otherwise reported as a shadow decision), and so a
	// trim cap is applied before the pool opens rather than by shedding after
	// it. Held proxies stay desired; the reload budget admits them when the cap
	// rises.
	bootID, oomKills := readOOMKillEpoch()
	for _, line := range oomCapDecide(len(allProxySettings), bootID, oomKills, time.Now()) {
		importantLogf("%s\n", line)
	}
	launchSettings := allProxySettings
	if trimCap, _, terr := effectiveTrimCapSource(); terr == nil && trimCap > 0 && len(allProxySettings) > trimCap {
		startupURLState, _ := readProxyURLState()
		gradeFor := buildTrimGradeResolver(proxyState, startupURLState)
		var held []*connect.ProxySettings
		launchSettings, held = startupTrimSelection(allProxySettings, trimCap, proxyState.Proxies, gradeFor,
			func(key string) float64 { return proxyEarningsScore(key, time.Now()) })
		importantLogf("[proxy][trim] startup: cap=%d, launching %d of %d desired, holding %d worst-graded until the cap is raised\n",
			trimCap, len(launchSettings), len(allProxySettings), len(held))
	}
	// Prime the reload loop's change-detector with the cap (and its source)
	// this start already saw and applied above, whether or not it bound (a
	// cap looser than the desired count still counts as "seen"). Without this
	// the first reload after a capped startup reads the same cap fresh and
	// treats it as new: a duplicate "[proxy][trim] received" line and a
	// duplicate ledger "applied" entry whose From is a partial mid-ramp
	// running count, even though startup already logged and applied it.
	if startupCap, startupSource, serr := effectiveTrimCapSource(); serr == nil && startupCap > 0 {
		primeTrimCapSeen(startupCap, startupSource)
	}
	// Store the launch count for the startup resource warning, which runs from
	// the first proxy goroutine (after the tier memory limits are applied) and
	// needs the pool size this start actually opens, not the desired count.
	resourceConfigLaunchCount.Store(int64(len(launchSettings)))
	// Startup holds no reload lock, so any critical-log line the cap lookup
	// queued (an unparseable proxy_trim) is written straight away here rather
	// than waiting for a reload that may be hours away.
	for _, line := range drainDeferredCrit() {
		critLog("%s", line)
	}
	{
		// Say once, at startup, what RAM ceiling this process tunes itself
		// against (see resource_config_warn.go). The short-pool warning that
		// used to sit here moved into the first launch goroutine, because the
		// soft memory limit it reports is only in force after the tier code
		// has run.
		ceilingBytes, ceilingSource := connectx.EffectiveRAMLimit()
		importantLogf("%s\n", ramCeilingLogLine(ceilingBytes, ceilingSource))
	}
	// Record what this start actually launched, for the next start's decision.
	oomCapRecordStart(len(launchSettings), bootID, oomKills, time.Now())
	proxySchedules, warmCount, renewableCount, coldCount := prioritizeAndScheduleProxies(launchSettings, proxySourceOf, currentNetworkId)
	tlog("[startup] proxy prioritization: %d total (warm: %d, renewable: %d, cold: %d)\n",
		len(launchSettings), warmCount, renewableCount, coldCount)

	if ranked, promoted, topAddr, topScore := earningsHistorySummary(allProxySettings, proxySourceOf, time.Now()); ranked == 0 {
		tlog("[startup] earnings ranking: no history yet\n")
	} else {
		tlog("[startup] earnings ranking: %d of %d proxies have earnings history, %d promoted, top earner %s at %s\n",
			ranked, len(allProxySettings), promoted, topAddr, formatBytes(uint64(topScore)))
	}

	// Start direct connection.
	directStartupDone := provideDirectSetup(st)

	// Launch per-proxy goroutines.
	proxyState.NextID = currentProxyIDCounter()
	if err := writeProxyState(proxyState); err != nil {
		tlog("[proxy] warning: could not write proxy.state: %v\n", err)
	}

	globalProxySlowRetryState.Store(LoadProxySlowRetryState())
	// Adopt legacy bare-address slow-retry entries onto identity keys so a
	// pre-upgrade proxy keeps its continuous 14-day drop clock.
	globalProxySlowRetryState.Load().adoptLegacy(allProxySettings)
	setConfiguredProxyCount(trimmedConfiguredCount(len(allProxySettings)))

	finishProxy := bannerPhase("Proxy load")
	if 0 < len(allProxySettings) {
		finishProxy(fmt.Sprintf("%d servers", len(allProxySettings)))

		for _, ps := range allProxySettings {
			key := ps.Key()
			stableID := resolveProxyID(proxyState, key)
			setProxyIndex(key, stableID)
			tagProxySourceIfUnset(proxyState, key, proxySourceOf[key])
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
			st.proxyCancelMap[proxySettings.Key()] = proxyCancel
			// Keyed by the same identity key as the cancel map entry above:
			// beginProxyLaunch stores the generation, and the readers that
			// compare it look the proxy up by identity. A bare address here
			// never matches, so a dead proxy's generation check fails and it
			// stays in the cancel map.
			proxyCtx = withProxyLaunchGen(proxyCtx, beginProxyLaunch(proxySettings.Key()))
			st.proxyCancelMu.Unlock()

			stableID := getProxyIndex(proxySettings.Key())
			isURLSourced := proxySourceOf[proxySettings.Key()] == "url"
			baseDelay := sched.Delay
			staggerDuration := sched.Stagger
			st.wg.Add(1)
			go connect.HandleError(func() {
				defer st.wg.Done()
				key := proxySettings.Key()
				gen := RegisterProxy(stableID, proxySettings.Address, key)
				defer UnregisterProxySafe(stableID, gen)
				defer proxyCancel()

				if !backoffPacerWithDelay(baseDelay, staggerDuration, proxyCtx) {
					return
				}
				if !isURLSourced && proxySettings != nil && globalProxySlowRetryState.Load().WasDropped(proxySettings.Key()) {
					dropAge := time.Since(globalProxySlowRetryState.Load().DropTime(proxySettings.Key()))
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
	tlog("📶 Ready — %s | profile=%s | proxies=%d\n", readyVersion, readyProfile, len(allProxySettings))

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
		directDone:      directStartupDone,
		networkID:       currentNetworkId,
	}
	// Seed runningAuth with the STARTUP launch settings BEFORE the watcher and
	// the first reload(). The startup loop above launched every proxy directly
	// (before the reloader existed), so an unseeded reload() would see no
	// recorded auth for any boot-launched proxy and rotate (cancel and
	// relaunch) all of them at boot. Seeding also has to precede the re-paste
	// case (LA7 incident: 100 proxies pasted with new creds, "added 100"
	// printed, daemon kept dialing the old user).
	reloader.seedRunningAuth(launchSettings)
	// Install the live `h3` re-apply hook BEFORE StartWatcher and the first
	// reload: the control socket is already live, and both StartWatcher and
	// reload() can drive reResolveActiveH3Cap, whose reconnect is inert unless
	// the hook is installed. Installing it here (rather than after reload())
	// also means the first reload's cap re-resolve reconnects the identities it
	// changes. See reapplyH3ModeLive.
	installH3ReapplyLive(func() { reapplyH3ModeLive(st) })
	// A hot swap execs in place: the pid survives and the previous image's
	// proxy.lock would read as held-by-a-live-holder. Clear it before any
	// reload path (watcher, watchdog, first reload) can observe it.
	cleanStaleSelfProxyLock()
	reloader.StartWatcher(st.ctx)
	go superviseLoop(st.ctx, "reload_watchdog", func() { reloader.RunReloadWatchdog(st.ctx) }, nil)
	reloader.reload()
	// Apply once now: a control update that landed while the startup loop was
	// launching identities would not otherwise reconnect the identities it
	// affects.
	reapplyH3ModeLive(st)

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
	// Direct-path (native local-IP) health grade: read-only visibility into the
	// box's own route, persisted to its own file so no consumer can act on it.
	go connect.HandleError(func() { runDirectGrader(st.ctx) })
	go connect.HandleError(func() { runProxyGradeSummary(st.ctx) })
	go connect.HandleError(func() { pruneURLProxyBlacklist(st.ctx) })
	go connect.HandleError(func() { runProxyURLCleanup(st.ctx, cleanupScope, cleanupInterval, selfHealEnabled) })
	// Supervised: a panic restarts the loop with backoff instead of ending it for
	// good (and leaving the last pressure score, GOGC and memory budget in force).
	if pressureLoopsSupported {
		go superviseLoop(st.ctx, "pressure_monitor", func() { runPressureMonitor(st.ctx, selfHealEnabled) }, nil)
		go superviseLoop(st.ctx, "pool_controller", func() { runPoolController(st.ctx, proxyURLMax, selfHealEnabled) }, nil)
	}
	go superviseLoop(st.ctx, "degraded_proxy_reaper", func() { runDegradedProxyReaper(st.ctx, st.proxyCancelMap, &st.proxyCancelMu) }, nil)
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
		enableProfilingWithRetry(st.ctx, profileAddr, EnableProfiling, 90*time.Second, time.Second, tlog)
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

// usesSlowRetrySemaphore reports whether this auth attempt must take a slot on
// the shared slow-retry semaphore, which caps how many failing paid or file
// proxies sit in the auth pipeline at once. The direct connection (the
// provider's own identity) and URL-sourced proxies never queue behind it. A
// proxy qualifies once its first ladder is exhausted (authFailures at the
// ceiling), while it is in the slow retry ramp, and after any genuine give-up:
// the genuine give-up pins authFailures one below the ceiling and zeroes
// slowRetryCycles, so genuineRetryCycles is what keeps every later ramp and
// daily attempt on the semaphore.
func usesSlowRetrySemaphore(hasProxy, isURLSourced bool, authFailures, maxAuthFailures, slowRetryCycles, genuineRetryCycles int) bool {
	if !hasProxy || isURLSourced {
		return false
	}
	return authFailures >= maxAuthFailures || 0 < slowRetryCycles || 0 < genuineRetryCycles
}
