package miner

// direct_slot.go owns the native direct slot of a running provide run.
//
// The slot is the nil-settings entry of the launch list (run.go): the box's own
// egress, no proxy identity and no ProxySettings. `provider direct on|off`
// (cmd_direct.go) persists <state>/direct and pokes the proxy reload trigger,
// but until this handle existed nothing in the running process applied the
// change: the toggle was read at startup and by the direct grader only, so the
// CLI printed DISABLED while the direct slot kept serving. That is the abuse
// report / legal takedown case where the toggle has to be real, not advisory.
//
// The handle is deliberately NOT part of the proxy reloader's registry: the
// direct slot is not a proxy identity, has no proxy failure history, and must
// never be diffed against proxy_url.json. It is one cancellable launch that the
// control path reaches through minerProxyReloader.onReload, and it runs the
// same provideWithProxy launch the startup loop runs (no second launch path).

import (
	"context"
	"sync"

	"github.com/urnetwork/connect"
)

// directSlot is the run's one handle on its native direct slot.
//
// It keeps a small controller goroutine (serve) alive for the whole run. The
// controller launches provideWithProxy while the operator toggle is on, cancels
// that launch when it goes off, and parks without returning while it stays off.
// Parking rather than returning matters: the run's join waits on its slots, so
// a direct-only provider whose slot simply exited would end the process and
// `provider direct on` would have nothing to start. Only an operator stop is
// looped over; any other end of a launch (yield, provider failure) ends the
// controller exactly as the fixed launch did before.
type directSlot struct {
	ctx    context.Context
	launch func(context.Context, uint64) error
	index  uint64
	// onStart narrates one launch (the run's provider_started event). It runs
	// on the controller goroutine, before the launch, once per start.
	onStart func(uint64)

	mu      sync.Mutex
	enabled bool
	// cancel is the live launch's cancel, nil whenever the slot is parked or
	// between launches.
	cancel context.CancelFunc
	// stopping marks the live launch as cancelled by the operator toggle. The
	// controller reads it after the launch returns: an operator stop is not a
	// provider failure and must not end the run.
	stopping bool
	// wake carries one pending state change to the parked controller.
	wake chan struct{}
}

func newDirectSlot(
	ctx context.Context,
	launch func(context.Context, uint64) error,
	index uint64,
	enabled bool,
	onStart func(uint64),
) *directSlot {
	return &directSlot{
		ctx:     ctx,
		launch:  launch,
		index:   index,
		onStart: onStart,
		enabled: enabled,
		wake:    make(chan struct{}, 1),
	}
}

// directSlotIndex returns the direct slot's position in the launch list and
// whether the run has one at all.
func directSlotIndex(providers []*connect.ProxySettings) (uint64, bool) {
	for index, settings := range providers {
		if settings == nil {
			return uint64(index), true
		}
	}
	return 0, false
}

// applyDirectToggle reconciles a run's direct slot with the persisted operator
// toggle, using the same precedence a restart uses (toggle file, then
// DISABLE_DIRECT_IP, then on). It is the one call the control path makes, so the
// CLI's `direct off` reaches the live slot through exactly the state it wrote. A
// nil handle (offline command, profile run, a run with no direct slot) means
// there is nothing to reconcile and the toggle only affects the next start.
func applyDirectToggle(slot *directSlot) bool {
	if slot == nil {
		return false
	}
	return slot.setEnabled(isDirectEnabled())
}

// setEnabled applies the operator's direct state. It reports whether the
// desired state changed, so calling it with the state already in force is a
// no-op: turning direct on twice cannot start two sessions, and turning it off
// twice cannot cancel a launch twice. A stop cancels the live launch but does
// not wait for it here; the controller serialises the launches, so a following
// "on" starts only after the previous launch has unwound.
func (d *directSlot) setEnabled(enabled bool) bool {
	d.mu.Lock()
	if d.enabled == enabled {
		d.mu.Unlock()
		return false
	}
	d.enabled = enabled
	var cancel context.CancelFunc
	if !enabled && d.cancel != nil {
		d.stopping = true
		cancel = d.cancel
	}
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if enabled {
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
	return true
}

// running reports whether a direct launch is live right now.
func (d *directSlot) running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel != nil
}

// enabledNow reports the current desired state.
func (d *directSlot) enabledNow() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.enabled
}

// serve is the run's direct-slot controller. It returns only when the run's
// context ends or a launch that the operator did not stop comes back with an
// error, so a provider failure still ends the run (and the supervisor retries)
// exactly as it did before the handle existed.
func (d *directSlot) serve() error {
	for {
		if !d.waitEnabled() {
			return nil
		}
		err, operatorStop := d.launchOnce()
		if operatorStop {
			if err != nil && d.ctx.Err() == nil {
				tlog("[direct] native [direct] transport stopped with an error (operator disabled): %v\n", err)
			}
			continue
		}
		if err != nil {
			return err
		}
		// The launch ended on its own. If the operator turned the slot off in
		// the meantime, park instead of ending the run: the process has to stay
		// up so a later `direct on` can start it again.
		if !d.enabledNow() {
			continue
		}
		return nil
	}
}

// waitEnabled blocks until the operator state is on. It returns false when the
// run context ends first.
func (d *directSlot) waitEnabled() bool {
	for {
		d.mu.Lock()
		if d.ctx.Err() != nil {
			d.mu.Unlock()
			return false
		}
		if d.enabled {
			d.mu.Unlock()
			return true
		}
		d.mu.Unlock()
		select {
		case <-d.ctx.Done():
			return false
		case <-d.wake:
		}
	}
}

// launchOnce runs one provideWithProxy launch to completion. It reports whether
// the end was an operator stop (which the controller loops over) as well as the
// launch's error, so the caller can tell "turned off" from "broke".
func (d *directSlot) launchOnce() (error, bool) {
	d.mu.Lock()
	if !d.enabled {
		// A stop landed between waitEnabled and here: do not start.
		d.mu.Unlock()
		return nil, true
	}
	launchCtx, cancel := context.WithCancel(d.ctx)
	d.cancel = cancel
	d.stopping = false
	index := d.index
	onStart := d.onStart
	d.mu.Unlock()

	if onStart != nil {
		onStart(index)
	}
	err := d.launch(launchCtx, index)

	d.mu.Lock()
	stopping := d.stopping
	d.cancel = nil
	d.stopping = false
	d.mu.Unlock()
	cancel()
	if stopping {
		tlog("[direct] native [direct] transport stopped (operator disabled)\n")
	}
	return err, stopping
}
