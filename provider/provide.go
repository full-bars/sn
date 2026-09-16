package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
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

func getProxyIndex(addr string) int {
	if v, ok := proxyIndexByAddr.Load(addr); ok {
		return v.(int)
	}
	return 0
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
	provideLaunchGoroutines(st)
	provideLauncherLoop(st)
	provideStatusServer(st)

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
	os.Exit(0)
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
		ResizeMessagePoolsPerClass(maxMemory / 8)
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
		defer st.hotSwapIPC.Close()
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

	defer flushRetentionEvents()

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
	globalControlState.shutdownFn = st.cancel

	if !st.isHotSwapCandidate {
		var err error
		st.cleanupControlSocket, err = startControlSocket(st.ctx, globalControlState)
		if err != nil {
			tlog("[control] failed to start control socket, urnet-tools will fall back to file-based overrides: %s\n", err)
		} else {
			defer func() {
				if st.cleanupControlSocket != nil {
					st.cleanupControlSocket()
				}
			}()
			unregSocketCloser := RegisterCoordinatorCloser(func() {
				if st.cleanupControlSocket != nil {
					st.cleanupControlSocket()
					st.cleanupControlSocket = nil
				}
			})
			defer unregSocketCloser()
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

	// Hourly pulse
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
				TriggerPulse()
			}
		}
	}()

	st.nodeName = strings.TrimSpace(os.Getenv("URNETWORK_NODE_NAME"))

	watcherName := st.nodeName
	if watcherName == "" {
		watcherName, err := os.Hostname()
		if err != nil || watcherName == "" {
			watcherName = "unknown"
		}
		if containerIDRe.MatchString(watcherName) {
			watcherName = "provider"
		}
	}

	go connect.HandleError(func() { runHealthHeartbeat(st.ctx, provideStartTime, os.Getenv("URNETWORK_PROFILE")) })
	go connect.HandleError(func() {
		runBandwidthReporter(st.ctx, watcherName, watcherName, os.Getenv("URNETWORK_REPORT_URL"), provideStartTime)
	})
	go connect.HandleError(func() {
		runHeartbeatReporter(st.ctx, watcherName, watcherName, os.Getenv("URNETWORK_REPORT_URL"), provideStartTime)
	})
	go connect.HandleError(func() { runJWTRefresher(st.ctx, st.apiUrl) })
	go connect.HandleError(func() { runEarningWindows(st.ctx) })
	go connect.HandleError(func() { runLifetimeCollector(st.ctx) })
	go connect.HandleError(func() { runProfitHeartbeat(st.ctx) })
	go connect.HandleError(func() { runBillableRateWriter(st.ctx) })

	go connect.HandleError(func() { paceMonitor(st.ctx) })
}

// provideWithProxy runs a single proxy's transport lifecycle.
func provideWithProxy(st *provideState, proxyCtx context.Context, proxySettings *connect.ProxySettings, isNative bool, isURLSourced bool) {
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ProxySettings = proxySettings
	clientSettings := connect.DefaultClientSettings()
	if seed, err := readProviderClientKeySeed(); err == nil && 0 < len(seed) {
		clientSettings.ClientKeySeed = seed
	}
	if certPem, keyPem, err := readProviderTlsCertAndKey(); err == nil && 0 < len(certPem) && 0 < len(keyPem) {
		if clientSettings.EncryptionSettings == nil {
			clientSettings.EncryptionSettings = connect.DefaultEncryptionSettings()
		}
		clientSettings.EncryptionSettings.ProvideTlsCertificatePem = certPem
		clientSettings.EncryptionSettings.ProvideTlsPrivateKeyPem = keyPem
	}
	enableProviderEncryption(clientSettings)
	localUserNatSettings := connect.DefaultLocalUserNatSettings()

	ApplyAutoTuning(clientSettings, localUserNatSettings)
	applyLowmodeSettings(clientSettings, localUserNatSettings)
	applyTurboSettings(clientSettings, localUserNatSettings)

	profile := os.Getenv("URNETWORK_PROFILE")
	applyTurboMemoryLimit(profile, st.maxMemory)
	applyEcoSettings(st.maxMemory)
	ensureMemoryLimit(st.maxMemory)
	localUserNatSettings.TcpBufferSettings.ConnectSettings = clientStrategySettings.ConnectSettings
	localUserNatSettings.UdpBufferSettings.ConnectSettings = clientStrategySettings.ConnectSettings
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
			if globalProxySlowRetryState.WasDropped(proxySettings.Address) || globalProxySlowRetryState.TimeUntilNextAttempt(proxySettings.Address) > 0 {
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
				admitFailureCount := authFailures
				if proxySettings != nil {
					admitFailureCount = globalProxyFailureHistory.FailureCount(proxySettings.Address)
				}
				if !isURLSourced && authFailures >= maxAuthFailures {
					select {
					case slowRetrySemaphore <- struct{}{}:
					case <-proxyCtx.Done():
						return "", connect.Id{}, false, proxyCtx.Err()
					}
				}
				release, waitErr := globalProxyAdmissionGate.Admit(proxyCtx, admitFailureCount)
				if waitErr != nil {
					if !isURLSourced && authFailures >= maxAuthFailures {
						<-slowRetrySemaphore
					}
					return "", connect.Id{}, false, waitErr
				}
				identityKey := "direct"
				if proxySettings != nil {
					identityKey = proxySettings.Address
				}
				byClientJwt, clientId, reused, err = provideAuth(proxyCtx, clientStrategy, st.apiUrl, st.opts, st.nodeName, identityKey)
				release()
				if !isURLSourced && authFailures >= maxAuthFailures {
					<-slowRetrySemaphore
				}
				if proxySettings != nil {
					if err == nil {
						globalProvenProxies.MarkSucceeded(proxySettings.Address)
						globalProxyFailureHistory.Reset(proxySettings.Address)
						globalProxySlowRetryState.ClearDropped(proxySettings.Address)
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
			}
			if authFailures >= maxAuthFailures {
				cause := classifyAuthFailureCause(err)
				if isURLSourced {
					return "", connect.Id{}, false, fmt.Errorf("authentication failed after %d attempts — %s: %w", maxAuthFailures, cause, err)
				}
				if proxySettings != nil {
					startedAt := globalProxySlowRetryState.RecordSlowRetryStart(proxySettings.Address)
					if globalProxySlowRetryState.ShouldDrop(proxySettings.Address) {
						globalProxySlowRetryState.MarkDropped(proxySettings.Address)
						dropAge := time.Since(startedAt)
						tlog("[proxy][slow-retry] proxy[%d] (%s) dropped after %s of continuous failure (%d total attempts)\n",
							getProxyIndex(proxySettings.Address), proxySettings.Address, formatDuration(dropAge), authFailures)
						st.proxyCancelMu.Lock()
						delete(st.proxyCancelMap, proxySettings.Address)
						st.proxyCancelMu.Unlock()
						return "", connect.Id{}, false, fmt.Errorf("proxy dropped after %s of continuous failure — %s", formatDuration(dropAge), cause)
					}
					slowRetryAttempt := authFailures - maxAuthFailures + 1
					if slowRetryAttempt > slowRetryRampAttempts && !globalProxySlowRetryState.RecordSlowRetryAttempt(proxySettings.Address) {
						waitTime := globalProxySlowRetryState.TimeUntilNextAttempt(proxySettings.Address)
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
		provideHandleAuthFailure(st, proxySettings, isNative, isURLSourced, err)
		return
	}

	identityKey := "direct"
	proxyIndex := 0
	if proxySettings != nil {
		identityKey = proxySettings.Address
		proxyIndex = getProxyIndex(proxySettings.Address)
	}

	instanceId := connect.NewId()

	oob := connect.NewApiOutOfBandControl(proxyCtx, clientStrategy, byClientJwt, st.apiUrl)
	connectClient := connect.NewClient(proxyCtx, clientId, oob, clientSettings)
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
	platformTransport := connect.NewPlatformTransportWithDefaults(proxyCtx, clientStrategy, connectClient.RouteManager(), st.connectUrl, auth)
	unregCloser := RegisterCoordinatorCloser(func() {
		platformTransport.Close()
	})
	defer unregCloser()

	proxyBecameLive()
	defer proxyWentDown()

	// HotSwap candidate ACK
	var unregSocketCloser func()
	st.candidateAckOnce.Do(func() {
		if st.isHotSwapCandidate && st.hotSwapIPC != nil {
			_ = runHotSwapChildAck(st.hotSwapIPC)
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
			startMetricsAfterTakeover(globalControlState)

			if cleanup, err := startControlSocket(st.ctx, globalControlState); err != nil {
				tlog("[control] candidate failed to start control socket on takeover: %s\n", err)
			} else {
				st.cleanupControlSocket = cleanup
				unregSocketCloser = RegisterCoordinatorCloser(func() {
					if st.cleanupControlSocket != nil {
						st.cleanupControlSocket()
						st.cleanupControlSocket = nil
					}
				})
			}
		}
		_ = notifySystemdReady()
	})
	if unregSocketCloser != nil {
		defer unregSocketCloser()
	}

	// Revocation watcher for reused identities.
	revocationDone := make(chan struct{})
	if reused {
		go watchReusedIdentityForRevocation(proxyCtx, identityKey, proxyIndex, revocationDone)
	}

	// In-process client-JWT renewal. Wrap OOB for the renewal watcher.
	renewalOOB := WrapRenewalOOB(oob)
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

	// Register bandwidth (tracks internally, bw not passed to connect in v2026).
	_ = RegisterProxyBandwidth(proxyIndex)

	// Note: NewLocalUserNat in v2026 no longer takes a bw parameter.
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
func provideHandleAuthFailure(st *provideState, proxySettings *connect.ProxySettings, isNative, isURLSourced bool, err error) {
	if proxySettings != nil {
		if isURLSourced {
			st.proxyCancelMu.Lock()
			delete(st.proxyCancelMap, proxySettings.Address)
			st.proxyCancelMu.Unlock()

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
			defer UnregisterProxy(0)

			RegisterProxy(0, "direct")
			provideWithProxy(st, nativeCtx, nil, true, false)
		})
	} else {
		tlog("[no-direct] providing on direct/local IP is disabled; using proxy list only\n")
	}
	return !noDirect
}

// provideLauncherLoop loads proxy state, launches per-proxy goroutines,
// starts the reloader, and runs URL fetcher goroutines.
func provideLauncherLoop(st *provideState) {
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

	_, closeDohCache := initPersistentDohCache(st.ctx)
	defer closeDohCache()

	globalProxySlowRetryState = LoadProxySlowRetryState()
	setConfiguredProxyCount(len(allProxySettings))

	finishProxy := bannerPhase("Proxy load")
	if 0 < len(allProxySettings) {
		finishProxy(fmt.Sprintf("%d servers", len(allProxySettings)))

		for _, ps := range allProxySettings {
			stableID := resolveProxyID(proxyState, ps.Address)
			setProxyIndex(ps.Address, stableID)
			tagProxySourceIfUnset(proxyState, ps.Address, proxySourceOf[ps.Address])
			RegisterProxy(stableID, ps.Address)
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
			st.proxyCancelMu.Unlock()

			stableID := getProxyIndex(proxySettings.Address)
			isURLSourced := proxySourceOf[proxySettings.Address] == "url"
			baseDelay := sched.Delay
			staggerDuration := sched.Stagger
			st.wg.Add(1)
			go connect.HandleError(func() {
				defer st.wg.Done()
				defer UnregisterProxy(stableID)
				defer proxyCancel()

				if !backoffPacerWithDelay(baseDelay, staggerDuration, proxyCtx) {
					return
				}
				if !isURLSourced && proxySettings != nil && globalProxySlowRetryState.WasDropped(proxySettings.Address) {
					dropAge := time.Since(globalProxySlowRetryState.DropTime(proxySettings.Address))
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
	go connect.HandleError(func() { runReloadReconciler(st.ctx) })

	// Profiling.
	if profileAddr := os.Getenv("URNETWORK_PPROF"); profileAddr != "" {
		tlog("[profile] enabling diagnostics on %s\n", profileAddr)
		if err := EnableProfiling(profileAddr); err != nil {
			tlog("[profile] failed: %v\n", err)
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
	if metricsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		metricsServer.Shutdown(ctx)
	}
	markCleanShutdown()
	if st.cleanupControlSocket != nil {
		st.cleanupControlSocket()
	}
}
