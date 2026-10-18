package miner

// hotswap.go ports the v33 provider's zero-downtime handoff
// (provider/hotswap.go, provider/hotswap_unix.go, provider/hotswap_windows.go,
// provider/hotswap_socketpair_*.go) onto the miner's one-program layout.
//
// The parent side spawns the binary at the install path as a candidate, talks
// the READY/TAKEOVER/ACK protocol over a private socketpair (Unix) or named
// pipe (Windows), hands MainPID over under systemd, and drains in-flight
// streams before exiting. In a container (PID 1 or /.dockerenv) there is no
// supervisor to re-parent the successor, so it execve's itself in place
// instead, leaving URNETWORK_HOTSWAP_EXEC behind so the successor's audited
// start still labels the handoff.
//
// Adaptations from the provider tree:
//   - The control-socket quiesce hook, recordHotSwapAndFlush,
//     HotSwapDrainTimeout and auditReconcileGrace are already declared in
//     package miner (proxy_manager_audit_ring.go), so the port shares them.
//     RequireVersion and flushRetentionEvents are likewise shared (run.go,
//     proxy_manager_health_log.go).
//   - The coordinator closer registry (RegisterCoordinatorCloser and friends)
//     lives here: the v33 tree keeps it beside the control socket, and the
//     miner's socket port predates the handoff.
//   - The state dir is the miner's providerStateDir (URNETWORK_STATE_DIR
//     honoured) rather than ~/.urnetwork, matching every other miner file.
//   - lifetimeStore is an empty seam: the v33 provider's lifetime metrics
//     (provider/lifetime_metrics.go) were not part of this port, so the
//     drain/execve paths flush nothing while keeping the call order.
//   - restoreStdioBeforeExec is the no-op the provider uses when its ramlog
//     pipe redirect is inactive; the miner does not redirect stdio.
//     (Superseded by the ported ramlog: the linux build restores the
//     pre-redirect descriptors via shmlog_linux.go, non-linux keeps this
//     no-op through shmlog_fallback.go.)

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docopt/docopt-go"
	gojwt "github.com/golang-jwt/jwt/v5"
)

// HotswapMsgType defines the message actions exchanged over the IPC channel.
type HotswapMsgType string

const (
	// HotswapMsgReady is sent by the candidate child once all pre-flight checks pass.
	HotswapMsgReady HotswapMsgType = "READY"

	// HotswapMsgTakeover is sent by the parent instructing the child to take over live traffic.
	HotswapMsgTakeover HotswapMsgType = "TAKEOVER"

	// HotswapMsgAck is sent by the child confirming active traffic takeover.
	HotswapMsgAck HotswapMsgType = "ACK"

	// HotswapMsgCanaryDone is sent by parent to canary child when running in Docker PID 1 mode
	// to signal that pre-flight succeeded and child should exit cleanly before parent execve.
	HotswapMsgCanaryDone HotswapMsgType = "CANARY_DONE"

	// HotswapMsgError is sent when either side encounters a fatal protocol or pre-flight error.
	HotswapMsgError HotswapMsgType = "ERROR"
)

// HotswapMessage represents the JSON payload framed across the hot-swap IPC channel.
type HotswapMessage struct {
	Type      HotswapMsgType `json:"type"`
	Version   string         `json:"version,omitempty"`
	PID       int            `json:"pid,omitempty"`
	Error     string         `json:"error,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

const (
	// EnvHotSwap is set to "1" in candidate child processes to indicate hot-swap mode.
	EnvHotSwap = "URNETWORK_HOTSWAP"

	// EnvHotSwapExec survives the in-place execve env strip so a Docker
	// successor still labels its start audit entry as a handoff.
	EnvHotSwapExec = "URNETWORK_HOTSWAP_EXEC"

	// HotSwapPreflightTimeout is the max duration the parent waits for the candidate's READY signal.
	HotSwapPreflightTimeout = 20 * time.Second

	// HotSwapAckTimeout is the max duration the parent waits for the candidate's ACK after TAKEOVER.
	HotSwapAckTimeout = 60 * time.Second

	// HotSwapDrainTimeout is declared in proxy_manager_audit_ring.go, beside
	// the audit-ring reconcile that waits past the same drain.
)

var (
	// ErrNoNotifySocket indicates that systemd's NOTIFY_SOCKET is not present.
	ErrNoNotifySocket = errors.New("NOTIFY_SOCKET not set")
)

var (
	// isHotSwapDraining tracks whether this process is currently draining in-flight streams.
	isHotSwapDraining atomic.Bool

	// hotSwapLock guards against concurrent hot-swap operations in the same process.
	hotSwapLock sync.Mutex

	// Test hook points
	getpidFunc                 = os.Getpid
	execInPlaceFunc            = execInPlace
	spawnCandidateFunc         = spawnHotSwapCandidate
	restoreStdioBeforeExecFunc = restoreStdioBeforeExec
	exitFunc                   = os.Exit
	executableFunc             = os.Executable
	statFunc                   = os.Stat
	syscallAccess              = checkExecAccess
)

// installPath is the executable path captured at process start, before any
// updater could move the running binary aside.
//
// os.Executable() reads /proc/self/exe, which follows the inode rather than the
// path. The update flow installs the new build at the provider's binary path
// and moves the running one to a backup, so from that moment os.Executable()
// returns the BACKUP path. Spawning that re-executes the old build: the handoff
// reports success, the operator sees a clean zero-downtime swap, and the
// provider keeps running the version it was already on.
//
// Capturing the path during init pins it while it still names the install
// location, so a handoff always launches whatever now lives there.
//
// On Linux, /proc/self/exe appends " (deleted)" when the running binary has
// been unlinked, yielding a path that does not exist on disk. We strip that
// suffix here so the captured install path always names the real file location.
var installPath = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(exe, procDeletedSuffix)
}()

// procDeletedSuffix is what readlink("/proc/self/exe") appends once the
// running binary has been unlinked. os.Executable returns it verbatim, with a
// nil error, and the resulting path does not exist.
const procDeletedSuffix = " (deleted)"

// checkExecutable reports whether path can actually be handed to exec. A
// plain existence check is not enough: an updater that has written the new
// build but not yet chmod'd it, or an install path replaced by a directory,
// both pass os.Stat and then fail at spawn. There is an unavoidable TOCTOU
// window between this and the spawn, so this is a way to fail early with a
// useful reason, not a guarantee.
//
// On non-Windows platforms, when a regular file exists but lacks execute
// permission, checkExecutable attempts the same self-healing chmod 0755 that
// spawnHotSwapCandidate performs on a spawn failure. This avoids a hard abort
// that would bypass the repair path and permanently block handoff.
func checkExecutable(path string) error {
	info, err := statFunc(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if runtime.GOOS != "windows" {
		// syscall.Access checks whether the calling process actually has
		// permission to execute the file, according to its real UID/GID.
		// The mode-bit mask (&0o111) only checks whether ANY execute bit
		// is set, not whether the caller can use it.
		if err := syscallAccess(path); err != nil {
			// Self-healing: if the file is a regular file but lacks
			// execute permission, repair it rather than aborting the
			// handoff. spawnHotSwapCandidate already does this on a
			// spawn failure, but healing here means the handoff can
			// proceed immediately.
			if errors.Is(err, os.ErrPermission) {
				hotswapLog("⚠️ [hotswap-heal] Install path %s missing execute permission; auto-healing chmod 0755...\n", path)
				if chmodErr := os.Chmod(path, 0o755); chmodErr != nil {
					return fmt.Errorf("%s is not executable and auto-heal chmod failed: %w", path, chmodErr)
				}
				return nil
			}
			return fmt.Errorf("%s is not executable: %w", path, err)
		}
	}
	return nil
}

// --- coordinator closers -----------------------------------------------------

// The coordinator closer registry is the single switch that hands the live
// coordinator session (every proxy's platform transport, the metrics listener,
// the control socket) from the retiring parent to its successor. A closer
// registered here is expected to be idempotent: yieldCoordinatorSession runs
// them all at once and the process normally exits shortly after.
var (
	coordinatorClosersMu  sync.Mutex
	coordinatorCloserSeq  uint64
	coordinatorClosersMap map[uint64]func()
)

// RegisterCoordinatorCloser registers closer and returns a function that
// unregisters it (safe to call more than once).
func RegisterCoordinatorCloser(closer func()) func() {
	if closer == nil {
		return func() {}
	}
	coordinatorClosersMu.Lock()
	defer coordinatorClosersMu.Unlock()
	coordinatorCloserSeq++
	id := coordinatorCloserSeq
	if coordinatorClosersMap == nil {
		coordinatorClosersMap = make(map[uint64]func())
	}
	coordinatorClosersMap[id] = closer

	var once sync.Once
	return func() {
		once.Do(func() {
			coordinatorClosersMu.Lock()
			delete(coordinatorClosersMap, id)
			coordinatorClosersMu.Unlock()
		})
	}
}

// ClearCoordinatorClosers clears all registered coordinator closers (primarily for testing).
func ClearCoordinatorClosers() {
	coordinatorClosersMu.Lock()
	defer coordinatorClosersMu.Unlock()
	coordinatorClosersMap = make(map[uint64]func())
}

// yieldCoordinatorSession executes all registered callbacks to disconnect from the coordinator.
func yieldCoordinatorSession() {
	coordinatorClosersMu.Lock()
	closers := make([]func(), 0, len(coordinatorClosersMap))
	for _, closer := range coordinatorClosersMap {
		if closer != nil {
			closers = append(closers, closer)
		}
	}
	coordinatorClosersMu.Unlock()

	for _, closer := range closers {
		closer()
	}
}

// --- lifetime metrics seam ---------------------------------------------------

// hotswapLifetimeStore is the process's lifetime-counter store, flushed at the
// two points a handoff bypasses the normal shutdown (the end of the parent's
// drain, and just before an in-place execve). The v33 provider's
// provider/lifetime_metrics.go did not come across with this port, so the
// miner flushes nothing here yet; the seam keeps those call sites and their
// ordering intact for the port that adds the counters.
type hotswapLifetimeStore struct{}

// Flush persists the lifetime counters. No-op until the counters are ported.
func (*hotswapLifetimeStore) Flush() {}

var lifetimeStore = &hotswapLifetimeStore{}

// restoreStdioBeforeExec is the per-OS implementation from the ported ramlog
// (shmlog_linux.go restores the pre-redirect descriptors for an in-place
// execve; shmlog_fallback.go is the no-op where there is no ramlog pipe).

// readyResult holds the result of reading a hotswap IPC message in a goroutine.
type readyResult struct {
	msg HotswapMessage
	err error
}

// handoffBatonSendTakeover sends TAKEOVER to the candidate and waits for its
// ACK, confirming the candidate has assumed live traffic. Returns nil on
// success, or kills the session and returns an error on failure/timeout.
func handoffBatonSendTakeover(session *HotswapParentSession, parentPID, childPID int) error {
	// Send TAKEOVER to candidate FIRST before yielding
	if err := writeHotswapMessage(session.Writer, HotswapMessage{
		Type:    HotswapMsgTakeover,
		PID:     parentPID,
		Version: RequireVersion(),
	}); err != nil {
		hotswapLog("❌ [hotswap] Failed to send TAKEOVER: %v. Aborting handoff; live provider retained.\n", err)
		session.Kill()
		return err
	}

	// Wait for mandatory ACK from candidate before yielding our own
	// coordinator session. The candidate has not taken over traffic yet,
	// so if the ACK fails or times out we can still abort with the
	// parent's transports untouched.
	ackCh := make(chan readyResult, 1)
	go func() {
		msg, err := readHotswapMessage(session.Reader)
		ackCh <- readyResult{msg, err}
	}()

	select {
	case res := <-ackCh:
		if res.err != nil || res.msg.Type != HotswapMsgAck {
			hotswapLog("❌ [hotswap] Candidate failed active takeover (%v). Aborting handoff; live provider retained.\n", res.err)
			session.Kill()
			return fmt.Errorf("candidate takeover unconfirmed: %v", res.err)
		}
		hotswapLog("♻️⚡ [hotswap] Candidate PID %d confirmed active takeover (ACK received)!\n", childPID)
	case <-time.After(HotSwapAckTimeout):
		hotswapLog("❌ [hotswap] Candidate takeover ACK timed out (>%s). Aborting handoff; live provider retained.\n", HotSwapAckTimeout)
		session.Kill()
		return fmt.Errorf("candidate takeover ACK timed out (>%s)", HotSwapAckTimeout)
	}

	return nil
}

// hotSwapDrainDone is closed when the parent's drain finishes, immediately
// before the drain's exit. A run whose slots unwound from the handoff's yield
// blocks on it, so the process outlives the drain window instead of exiting the
// moment the last slot returns.
var (
	hotSwapDrainMu   sync.Mutex
	hotSwapDrainDone chan struct{}
)

// waitForHotSwapDrain blocks while a handoff drain is in progress. The drain
// owns the exit (exitFunc), so this normally never returns.
func waitForHotSwapDrain() {
	hotSwapDrainMu.Lock()
	done := hotSwapDrainDone
	hotSwapDrainMu.Unlock()
	if done != nil {
		<-done
	}
}

func beginHotSwapDrain() chan struct{} {
	hotSwapDrainMu.Lock()
	defer hotSwapDrainMu.Unlock()
	hotSwapDrainDone = make(chan struct{})
	return hotSwapDrainDone
}

func finishHotSwapDrain(done chan struct{}) {
	if done == nil {
		return
	}
	hotSwapDrainMu.Lock()
	defer hotSwapDrainMu.Unlock()
	select {
	case <-done:
		// already closed
	default:
		close(done)
	}
}

// handoffDrainParent runs the common graceful-drain-then-exit logic shared by
// both the Windows baton handoff and the Unix systemd handoff. It monitors the
// candidate's liveness during the drain window, flushes retention events, and
// exits cleanly (or non-zero if the candidate dies prematurely).
func handoffDrainParent(ctx context.Context, cancel context.CancelFunc, session *HotswapParentSession, parentPID int) {
	isHotSwapDraining.Store(true)
	drainDone := beginHotSwapDrain()
	hotswapLog("♻️⚡ [hotswap] Parent PID %d entering graceful stream drain (max %s)...\n", parentPID, HotSwapDrainTimeout)

	go func() {
		// Monitor candidate child liveness during drain.
		//
		// Only when a real child process backs the session: Wait returns
		// immediately otherwise, which makes this case ready at once and
		// leaves the select below picking at random between it and
		// ctx.Done() — reporting a healthy handoff as a dead candidate and
		// exiting non-zero. A nil channel blocks forever, so the select then
		// resolves on the drain timeout or ctx.Done() alone, which is the
		// intended behaviour when there is no candidate process to outlive.
		var childWaitCh <-chan error
		if session.hasChildProcess() {
			ch := make(chan error, 1)
			go func() {
				ch <- session.Wait()
			}()
			childWaitCh = ch
		}

		candidateDied := false
		select {
		case <-time.After(HotSwapDrainTimeout):
			// Normal drain duration elapsed
		case <-childWaitCh:
			hotswapLog("⚠️ [hotswap] Candidate process exited unexpectedly during parent drain!\n")
			candidateDied = true
		case <-ctx.Done():
		}

		flushRetentionEvents()
		// Parent exits via os.Exit here, which skips the graceful shutdown
		// persist: push any entries recorded during the drain window to
		// disk. The successor merges audit.json after takeover, so this is
		// what carries the parent's final entries forward.
		forceAuditPersist()
		lifetimeStore.Flush()
		isHotSwapDraining.Store(false)
		cancel()
		// Release a run that unwound its slots at the yield; it must not exit
		// before the drain window has elapsed.
		finishHotSwapDrain(drainDone)
		if candidateDied {
			// The candidate already holds MainPID/traffic ownership; it died before
			// taking over, so no process is left serving. Exit non-zero so systemd
			// Restart=on-failure (or the Docker supervision loop) restarts the unit
			// instead of leaving the node silently offline after a clean exit(0).
			critLog("FATAL: hotswap candidate died during parent drain; exiting non-zero for supervisor restart")
			exitFunc(1)
			return
		}
		hotswapLog("♻️⚡ [hotswap] Graceful drain complete -> parent PID %d exiting cleanly.\n", parentPID)
		exitFunc(0)
	}()
}

// hotSwapExecutablePath returns the binary a handoff should launch: the path
// captured at startup, which is where an updater installs the new build.
//
// When that path is unusable the handoff aborts rather than falling back to
// the running image. A handoff exists to change version; re-executing the
// build we are trying to replace, while reporting a clean zero-downtime swap,
// is the exact failure this function was written to remove. Aborting leaves
// the live provider untouched so the updater's verify loop detects the
// mismatch and rolls back from backup rather than silently re-executing the
// same build.
//
// Known limitation: os.Executable resolves /proc/self/exe to the real inode,
// so a deployment that launches the provider through a rotating symlink pins
// the release directory it resolved to, not the symlink. Replacing the file at
// the install path (what the updater does) is handled; rotating a symlink
// above it is not.
func hotSwapExecutablePath() (string, error) {
	rawCurrent, currentErr := executableFunc()
	isDeleted := false
	var current string
	if currentErr == nil {
		isDeleted = strings.HasSuffix(rawCurrent, procDeletedSuffix)
		current = strings.TrimSuffix(rawCurrent, procDeletedSuffix)
	}

	if installPath == "" {
		if currentErr != nil {
			return "", fmt.Errorf("no executable path captured at startup and the running image is unresolvable: %w", currentErr)
		}
		if err := checkExecutable(current); err != nil {
			return "", fmt.Errorf("no executable path captured at startup and the running image %s is unusable: %w", current, err)
		}
		return current, nil
	}

	if err := checkExecutable(installPath); err != nil {
		return "", fmt.Errorf("install path %s is unusable, so this handoff could not change version: %w", installPath, err)
	}

	if currentErr == nil && (isDeleted || current != installPath) {
		if isDeleted {
			hotswapLog("⚡ [hotswap] Running image was unlinked on disk; launching the install path %s instead.\n", installPath)
		} else {
			// The running binary was moved aside, which is exactly what an update
			// does. Launching the install path is the whole point of the handoff.
			hotswapLog("⚡ [hotswap] Running image moved to %s; launching the install path %s instead.\n", current, installPath)
		}
	}
	return installPath, nil
}

// sanitizeCandidateArgs strips identity-mutating arguments from the parent's
// argv so the HotSwap candidate never re-authenticates or re-executes
// auth-provide with a stale auth code (F-3). Returns the cleaned argument list.
// Shared by the spawned-candidate path (hotswap_unix.go) and the Docker
// in-place execve branch below, so it lives in the platform-neutral file.
func sanitizeCandidateArgs(args []string) []string {
	var cleanArgs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "auth-provide" {
			cleanArgs = append(cleanArgs, "provide")
			// If followed by positional auth code, skip it
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		if arg == "-f" {
			continue
		}
		if arg == "--user_auth" {
			// Skip the flag and its positional value (separated form: --user_auth <val>)
			i++
			continue
		}
		if arg == "--password" {
			// Skip the flag and its positional value (separated form: --password <val>)
			i++
			continue
		}
		if strings.HasPrefix(arg, "--user_auth=") || strings.HasPrefix(arg, "--password=") {
			continue
		}
		cleanArgs = append(cleanArgs, arg)
	}
	return cleanArgs
}

// writeHotswapMessage serializes and writes a newline-delimited JSON message to w.
func writeHotswapMessage(w io.Writer, msg HotswapMessage) error {
	msg.Timestamp = time.Now()
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal hotswap message: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

// readHotswapMessage reads and deserializes a single newline-delimited JSON message from r.
func readHotswapMessage(r *bufio.Reader) (HotswapMessage, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return HotswapMessage{}, err
	}
	var msg HotswapMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return HotswapMessage{}, fmt.Errorf("unmarshal hotswap message %q: %w", string(line), err)
	}
	return msg, nil
}

// runHotSwapChildPreflight validates the tokens and configs a candidate needs
// before it announces READY: the account sign-in token exists, is readable and
// is not expired, and the API host answers a dial.
func runHotSwapChildPreflight(opts docopt.Opts, apiUrl string) error {
	// 1. Verify account JWT exists and is readable with a valid expiry. The
	// miner reads its own state dir (URNETWORK_STATE_DIR honoured), which is
	// where its network token renewer rotates the file.
	jwtPath, err := providerStatePath("jwt")
	if err != nil {
		return fmt.Errorf("resolve the network token path: %w", err)
	}
	jwtBytes, err := os.ReadFile(jwtPath)
	if err != nil {
		if os.IsPermission(err) {
			hotswapLog("⚠️ [hotswap-heal] Repairing permissions on %s (0600)\n", jwtPath)
			_ = os.Chmod(jwtPath, 0600)
			jwtBytes, err = os.ReadFile(jwtPath)
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", jwtPath, err)
		}
	}
	exp := parseJWTExpiryTime(string(jwtBytes))
	if exp == nil {
		return fmt.Errorf("account JWT at %s lacks a valid exp claim", jwtPath)
	}
	if time.Until(*exp) <= 0 {
		return fmt.Errorf("account JWT is expired (%s ago)", time.Since(*exp).Round(time.Second))
	}

	// 2. Verify API URL reachability (host:port dial) with dual-stack retry
	u, err := url.Parse(apiUrl)
	if err != nil {
		return fmt.Errorf("parse api_url %q: %w", apiUrl, err)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "http" {
			host += ":80"
		} else {
			host += ":443"
		}
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	var lastDialErr error
	for attempt := 1; attempt <= 3; attempt++ {
		conn, err := dialer.Dial("tcp", host)
		if err == nil {
			_ = conn.Close()
			lastDialErr = nil
			break
		}
		lastDialErr = err

		// Dual-stack healing: If IPv6 route is blackholed, try explicit IPv4
		conn4, err4 := dialer.Dial("tcp4", host)
		if err4 == nil {
			_ = conn4.Close()
			hotswapLog("⚡ [hotswap-heal] IPv4 fallback dial to %s succeeded on attempt %d\n", host, attempt)
			lastDialErr = nil
			break
		}
		lastDialErr = err4

		if attempt < 3 {
			time.Sleep(time.Duration(attempt*250) * time.Millisecond)
		}
	}
	if lastDialErr != nil {
		return fmt.Errorf("pre-flight dial to API %s failed: %w", host, lastDialErr)
	}

	return nil
}

// runHotSwapChildHandshake executes the child protocol: pre-flight checks -> announce READY -> wait TAKEOVER.
func runHotSwapChildHandshake(ipcConn io.ReadWriter, opts docopt.Opts, apiUrl string) error {
	hotswapLog("⚡ [hotswap] Candidate process (PID %d, version %s) running pre-flight checks...\n", os.Getpid(), RequireVersion())

	if err := runHotSwapChildPreflight(opts, apiUrl); err != nil {
		_ = writeHotswapMessage(ipcConn, HotswapMessage{
			Type:    HotswapMsgError,
			PID:     os.Getpid(),
			Version: RequireVersion(),
			Error:   err.Error(),
		})
		return err
	}

	// Announce READY to parent
	if err := writeHotswapMessage(ipcConn, HotswapMessage{
		Type:    HotswapMsgReady,
		PID:     os.Getpid(),
		Version: RequireVersion(),
	}); err != nil {
		return fmt.Errorf("send READY: %w", err)
	}

	hotswapLog("⚡ [hotswap] Candidate PID %d passed pre-flight -> announced READY to parent\n", os.Getpid())

	// Wait for response from parent: TAKEOVER (standard) or CANARY_DONE (Docker PID 1)
	reader := bufio.NewReader(ipcConn)
	msg, err := readHotswapMessage(reader)
	if err != nil {
		return fmt.Errorf("wait for parent handoff signal: %w", err)
	}
	switch msg.Type {
	case HotswapMsgTakeover:
		hotswapLog("⚡ [hotswap] TAKEOVER received from parent (PID %d) -> candidate assuming live traffic\n", msg.PID)
		return nil
	case HotswapMsgCanaryDone:
		hotswapLog("⚡ [hotswap] Canary verification confirmed by parent (PID %d) -> exiting cleanly for PID 1 takeover\n", msg.PID)
		exitFunc(0)
		return nil
	default:
		return fmt.Errorf("unexpected message from parent (wanted TAKEOVER or CANARY_DONE, got %s: %s)", msg.Type, msg.Error)
	}
}

// runHotSwapChildAck informs the parent that the child has assumed live traffic.
func runHotSwapChildAck(ipcConn io.Writer) error {
	return writeHotswapMessage(ipcConn, HotswapMessage{
		Type:    HotswapMsgAck,
		PID:     os.Getpid(),
		Version: RequireVersion(),
	})
}

// runHotSwapParentHandoff coordinates spawning the candidate, validating its readiness,
// yielding the coordinator session, and entering graceful stream drain (or in-place execve for PID 1).
func runHotSwapParentHandoff(ctx context.Context, cancel context.CancelFunc, opts docopt.Opts) error {
	if !hotSwapLock.TryLock() {
		hotswapLog("⚠️ [hotswap] Hot-swap already in progress; ignoring duplicate trigger\n")
		return nil
	}
	defer hotSwapLock.Unlock()

	if isHotSwapDraining.Load() {
		hotswapLog("⚠️ [hotswap] Process is already draining; ignoring trigger\n")
		return nil
	}

	exe, err := hotSwapExecutablePath()
	if err != nil {
		hotswapLog("❌ [hotswap] Failed to resolve executable path: %v. Aborting handoff.\n", err)
		return fmt.Errorf("resolve executable: %w", err)
	}

	parentPID := getpidFunc()
	hotswapLog("⚡ [hotswap] Initiating zero-downtime handoff (live parent PID %d, binary %s)...\n", parentPID, exe)

	session, err := spawnCandidateFunc(exe, os.Args[1:])
	if err != nil {
		hotswapLog("❌ [hotswap] Failed to spawn candidate: %v. Live provider retained untouched.\n", err)
		return err
	}
	defer session.Close()

	// Wait for READY from candidate with timeout
	readyCh := make(chan readyResult, 1)
	go func() {
		msg, err := readHotswapMessage(session.Reader)
		readyCh <- readyResult{msg, err}
	}()

	var childPID int
	select {
	case res := <-readyCh:
		if res.err != nil {
			hotswapLog("❌ [hotswap] Candidate failed pre-flight or disconnected: %v. Aborting handoff; live provider retained.\n", res.err)
			session.Kill()
			return res.err
		}
		if res.msg.Type != HotswapMsgReady {
			hotswapLog("❌ [hotswap] Candidate pre-flight failed (%s: %s). Aborting handoff; live provider retained.\n", res.msg.Type, res.msg.Error)
			session.Kill()
			return fmt.Errorf("candidate pre-flight failed: %s", res.msg.Error)
		}
		childPID = res.msg.PID
		hotswapLog("⚡ [hotswap] Candidate PID %d reported READY (version=%s)\n", childPID, res.msg.Version)
	case <-time.After(HotSwapPreflightTimeout):
		hotswapLog("❌ [hotswap] Candidate timed out during pre-flight (>%s). Aborting handoff; live provider retained.\n", HotSwapPreflightTimeout)
		session.Kill()
		return errors.New("candidate pre-flight timeout")
	case <-ctx.Done():
		session.Kill()
		return ctx.Err()
	}

	// Branch: Windows — Baton handoff (no execve, no systemd)
	// On Windows, the candidate takes over traffic via named-pipe IPC, then the
	// parent yields its coordinator session and drains in-flight streams before
	// exiting. There is no Docker PID-1 execve path and no systemd to notify.
	if runtime.GOOS == "windows" {
		hotswapLog("⚡ [hotswap] Windows detected: candidate pre-flight verified -> baton handoff\n")

		// Commit the handoff (record + flush) BEFORE sending takeover, so
		// the candidate's post-ACK merge reads a disk that already holds
		// the event. A rare phantom entry on takeover failure is the
		// accepted tradeoff for an airtight flush-before-merge ordering.
		recordHotSwapAndFlush()

		if err := handoffBatonSendTakeover(session, parentPID, childPID); err != nil {
			return err
		}
		yieldCoordinatorSession()
		handoffDrainParent(ctx, cancel, session, parentPID)
		return nil
	}

	// Branch: Docker Container (PID 1 or container environment) -> In-Place execve
	isDocker := false
	if _, err := os.Stat("/.dockerenv"); err == nil {
		isDocker = true
	}
	if getpidFunc() == 1 || isDocker {
		hotswapLog("⚡ [hotswap] Docker container detected: candidate pre-flight verified -> preparing in-place execve\n")

		// 1. Tell canary child it's done so it exits cleanly (with bounded timeout)
		if err := writeHotswapMessage(session.Writer, HotswapMessage{
			Type:    HotswapMsgCanaryDone,
			PID:     parentPID,
			Version: RequireVersion(),
		}); err != nil {
			hotswapLog("⚠️ [hotswap] Failed to send CANARY_DONE: %v\n", err)
		}

		waitCh := make(chan error, 1)
		go func() {
			waitCh <- session.Wait()
		}()
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
			hotswapLog("⚠️ [hotswap] Canary did not exit within 5s; terminating canary\n")
			session.Kill()
		}
		session.Close()

		// 2. Commit the handoff: record+flush before execve — execve
		// replaces the process image, so the in-memory audit ring vanishes
		// with it, and the new image's start label reads
		// URNETWORK_HOTSWAP_EXEC to mark the successor as such.
		recordHotSwapAndFlush()
		yieldCoordinatorSession()
		flushRetentionEvents()
		lifetimeStore.Flush()

		hotswapLog("⚡ [hotswap] Executing in-place syscall.Exec (container stays up)...\n")

		// 3. In-place execve: replaces process memory image without altering PID or closing stdout/stderr
		var cleanEnv []string
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, EnvHotSwap+"=") && !strings.HasPrefix(e, EnvHotSwapExec+"=") {
				cleanEnv = append(cleanEnv, e)
			}
		}
		// The execve'd successor is not a candidate (no IPC descriptor), but it
		// IS a handoff successor: keep a marker so its audited start is
		// labelled hotswap, not boot. Re-appended so a second in-place hotswap
		// of the same successor does not accumulate duplicates.
		cleanEnv = append(cleanEnv, EnvHotSwapExec+"=1")

		// The canary was spawned with sanitizeCandidateArgs, and the in-place
		// image must be too. A container first started as
		// `auth-provide <code>` would otherwise re-execute with an auth code
		// the first run already consumed, and exit.
		argv0 := exe
		var rest []string
		if len(os.Args) > 0 {
			argv0 = os.Args[0]
			rest = os.Args[1:]
		}
		args := append([]string{argv0}, sanitizeCandidateArgs(rest)...)

		// Hand stdout/stderr back from the ramlog pipe: its reader is a
		// goroutine in this process and does not survive exec, so the new
		// image would die of SIGPIPE on its first write.
		restoreStdioBeforeExecFunc()

		if err := execInPlaceFunc(exe, args, cleanEnv); err != nil {
			hotswapLog("CRITICAL [hotswap] syscall.Exec failed: %v\n", err)
			critLog("FATAL: syscall.Exec failed during hotswap: %v", err)
			exitFunc(1)
			return fmt.Errorf("syscall.Exec: %w", err)
		}
		return nil
	}

	// Standard Unix Branch (Host / systemd): Baton handoff to child
	// Pre-check systemd notify capability: if running under systemd, NOTIFY_SOCKET is required to update MainPID (F-2)
	if os.Getenv("INVOCATION_ID") != "" && os.Getenv("NOTIFY_SOCKET") == "" {
		msg := "zero-downtime hotswap unavailable: the provider's systemd unit is Type=simple (NOTIFY_SOCKET not set), so MainPID cannot be handed to a new process; `urnet-tools update` migrates the unit to Type=notify, after which updates can hot swap"
		hotswapLog("❌ [hotswap] %s\n", msg)
		session.Kill()
		return fmt.Errorf("%s: %w", msg, ErrNoNotifySocket)
	}

	// Every abort check has passed: commit the handoff — record it and
	// flush BEFORE the takeover message so the successor's post-ACK merge
	// reads a disk that already contains this event.
	recordHotSwapAndFlush()

	if err := handoffBatonSendTakeover(session, parentPID, childPID); err != nil {
		return err
	}

	// Yield the live coordinator session: the candidate has confirmed
	// active takeover, so there is no window where neither process holds it.
	yieldCoordinatorSession()

	// Update systemd service manager with new MainPID (if running under systemd)
	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != "" {
		if err := notifySystemdMainPID(childPID); err != nil {
			if errors.Is(err, ErrNoNotifySocket) {
				hotswapLog("⚠️ [hotswap] Running under systemd without Type=notify (NOTIFY_SOCKET unset). MainPID update skipped.\n")
			} else {
				hotswapLog("⚠️ [hotswap] systemd notify: %v\n", err)
			}
		} else {
			hotswapLog("⚡ [hotswap] systemd updated: MAINPID=%d\n", childPID)
		}
	}

	handoffDrainParent(ctx, cancel, session, parentPID)
	return nil
}

// candidateAckOnce makes the successor ACK the parent exactly once: the ack is
// sent by whichever slot first proves the takeover real, and the continuation
// it triggers (control socket rebind, audit merge, signal listener) must run
// once for the process.
var candidateAckOnce sync.Once

// waitForControlSocketRelease blocks until the control socket path stops
// answering, i.e. the retiring parent has quiesced it at the commit point. The
// successor cannot bind its own socket before that. A path that never answered
// (parent already gone, or a socket this process never bound) returns
// immediately; the timeout is a backstop so a wedged parent cannot hold the
// successor's takeover forever.
func waitForControlSocketRelease(timeout time.Duration) {
	path, err := controlSocketPath()
	if err != nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.Dial("unix", path)
		if err != nil {
			return
		}
		conn.Close()
		if time.Now().After(deadline) {
			tlog("[control] timed out waiting for parent to release control socket after takeover; proceeding anyway\n")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// hotSwapControlSocketOwner owns the process's control socket across a
// handoff. The parent binds it at startup and quiesces it at the commit point;
// a spawned candidate binds it only once the parent's is gone, from its
// takeover continuation. finish stops whatever this process still owns, in
// whichever role it ended up in.
type hotSwapControlSocketOwner struct {
	mu     sync.Mutex
	ctx    context.Context
	state  *controlState
	closer func()
	unreg  func()
}

// start binds the control socket if this process does not own one yet and
// installs the handoff's quiesce hook and coordinator closer for it.
func (o *hotSwapControlSocketOwner) start() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closer != nil {
		return nil
	}
	cleanup, err := startControlSocket(o.ctx, o.state)
	if err != nil {
		return err
	}
	o.closer = cleanup
	// The hotswap commit point quiesces (closes) this socket so no command
	// accepted after the audit flush can be lost in the parent's memory. The
	// wrapper nils the closure on the way out, so a later graceful exit cannot
	// clean up again and delete a successor's freshly bound socket.
	setQuiesceHook(func() { o.quiesce() })
	o.unreg = RegisterCoordinatorCloser(func() { o.quiesce() })
	return nil
}

// quiesce closes this process's control socket once, if it still owns one.
func (o *hotSwapControlSocketOwner) quiesce() {
	o.mu.Lock()
	closer := o.closer
	o.closer = nil
	o.mu.Unlock()
	if closer != nil {
		closer()
	}
}

// finish releases whatever this process still owns at run exit. A handoff may
// have moved the socket to the successor already, in which case there is
// nothing left to close.
func (o *hotSwapControlSocketOwner) finish() {
	o.mu.Lock()
	unreg := o.unreg
	o.unreg = nil
	o.mu.Unlock()
	if unreg != nil {
		unreg()
	}
	o.quiesce()
	setQuiesceHook(nil)
	forceControlAuditPersist()
}

// hotSwapTakeoverCandidate runs the successor's side of a completed handoff as
// soon as its first slot proves the takeover real. It mirrors the v33
// provider's candidate continuation (provider/provide.go): ACK the parent so it
// may yield and drain, wait for the parent's control socket to be released,
// pull the parent's final audit entries into this process's ring, bind this
// process's own control socket, and take over the SIGNAL and control-socket
// handoff triggers.
//
// The parent keeps the socket and the audit file until its commit point, which
// precedes its drain, so nothing here may run before the ACK.
func (self providerRunSettings) hotSwapTakeoverCandidate(ctx context.Context, cancel context.CancelFunc, controlSocket *hotSwapControlSocketOwner) {
	if self.hotSwapIPC != nil {
		_ = runHotSwapChildAck(self.hotSwapIPC) // fire-and-forget IPC ack
		_ = self.hotSwapIPC.Close()
	}

	waitForControlSocketRelease(HotSwapAckTimeout + 15*time.Second)

	// The parent flushed its final audit entries before the takeover message;
	// this ring was loaded at spawn time and predates that write, so pull them
	// in now. Without this the handoff event and the last control-socket
	// commands would exist only on disk and be dropped by the next persist.
	mergeAuditRingFromDisk()
	// The parent persists again at the end of its drain, from a ring without
	// our entries. Re-merge and persist once that is over so a crash soon after
	// takeover cannot lose our start entry.
	go reconcileAuditRingAfterHandoff(ctx, HotSwapDrainTimeout+auditReconcileGrace)

	// This process is now the provider: bind the socket the parent quiesced,
	// and answer SIGUSR2 (and the control socket's hotswap command) ourselves.
	if err := controlSocket.start(); err != nil {
		tlog("[control] candidate failed to start control socket on takeover: %s\n", err)
		// Do not carry the parent's socket closer into the hotswap commit
		// point: this process never bound that socket.
		setQuiesceHook(nil)
	}
	startHotSwapSignalListener(ctx, cancel, self.opts)
	setHotSwapTrigger(func() error {
		return runHotSwapParentHandoff(ctx, cancel, self.opts)
	})
}

// ResetHotSwapStateForTest resets draining state and closers for test isolation.
func ResetHotSwapStateForTest() {
	isHotSwapDraining.Store(false)
	ClearCoordinatorClosers()
	candidateAckOnce = sync.Once{}
	hotSwapDrainMu.Lock()
	hotSwapDrainDone = nil
	hotSwapDrainMu.Unlock()
}

// hotswapStateDir returns the miner's state dir (URNETWORK_STATE_DIR
// honoured), or "" when it cannot be resolved.
func hotswapStateDir() string {
	dir, err := providerStateDir()
	if err != nil || dir == "" {
		return ""
	}
	return dir
}

// readHotswapDeclinesFromDisk reads the hotswap decline/success counters
// that the urnet-tools CLI writes to <stateDir>/.hotswap_declines.json.
// Returns nil when the file is absent or unreadable.
func readHotswapDeclinesFromDisk() map[string]int64 {
	stateDir := hotswapStateDir()
	if stateDir == "" {
		return nil
	}
	path := filepath.Join(stateDir, ".hotswap_declines.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var dc struct {
		Counts map[string]int64 `json:"counts"`
	}
	if err := json.Unmarshal(data, &dc); err != nil || dc.Counts == nil {
		return nil
	}
	return dc.Counts
}

// parseJWTExpiryTime extracts the exp claim from a JWT without signature
// verification. This is intentional: the JWTs parsed here are read from local
// disk and are used only for hotswap timing diagnostics, not for authorization.
// DO NOT reuse this function on network-supplied JWTs where signature
// verification is required (M9 security note).
func parseJWTExpiryTime(byJwt string) *time.Time {
	parser := gojwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, gojwt.MapClaims{})
	if err != nil {
		return nil
	}
	claims, ok := tok.Claims.(gojwt.MapClaims)
	if !ok {
		return nil
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return nil
	}
	t := time.Unix(int64(exp), 0)
	return &t
}

func hotswapLog(format string, args ...any) {
	fmt.Printf("%s "+format, append([]any{time.Now().Format("0102 15:04:05")}, args...)...)
}
