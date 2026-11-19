package miner

import (
	"context"
	"strings"
	"sync"
	"time"
)

// extenderToggle serializes the operator's setting with the activation budget.
// Only the direct device registers. A stopped budget check cannot release a
// newer operator hold because both paths take the same lock.
type extenderToggle struct {
	mu           sync.Mutex
	device       extenderToggleDevice
	allow        func() bool
	cancel       context.CancelFunc
	generation   uint64
	settingKnown bool
	enabled      bool
	budgetHeld   bool
}
type extenderToggleDevice interface {
	HoldProvideExtenderSession(bool)
	SetProvideExtender(bool)
}

var globalExtenderToggle extenderToggle

func extenderSettingEnabled(state *controlState) bool {
	v, ok := state.get("extender")
	return !ok || !strings.EqualFold(v, "off")
}
func (m *extenderToggle) attach(device extenderToggleDevice, gate *extenderActivationGate) func() {
	m.mu.Lock()
	m.device, m.allow = device, gate.allowLaunch
	m.settingKnown = false
	m.reconcileLocked(extenderSettingEnabled(globalControlState))
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.device == device {
			m.stopCheckLocked()
			m.device = nil
			m.allow = nil
		}
	}
}
func (m *extenderToggle) stopCheckLocked() {
	m.generation++
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}
func (m *extenderToggle) reconcile(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconcileLocked(enabled)
}
func (m *extenderToggle) reconcileLocked(enabled bool) {
	if !enabled {
		publishExtenderHeadStartRole(false)
	}
	if m.device == nil {
		return
	}
	if m.settingKnown && m.enabled == enabled {
		return
	}
	m.settingKnown, m.enabled = true, enabled
	m.budgetHeld = false
	m.stopCheckLocked()
	if !enabled {
		m.device.HoldProvideExtenderSession(true)
		m.device.SetProvideExtender(false)
		publishExtenderHeadStartRole(false)
		return
	}
	allowed := m.allow()
	m.budgetHeld = !allowed
	m.device.HoldProvideExtenderSession(!allowed)
	m.device.SetProvideExtender(true)
	publishExtenderHeadStartRole(allowed)
	if allowed {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	generation := m.generation
	go func() {
		ticker := time.NewTicker(extenderBudgetRecheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			m.mu.Lock()
			if m.generation != generation || m.device == nil {
				m.mu.Unlock()
				return
			}
			if m.allow() {
				m.device.HoldProvideExtenderSession(false)
				m.budgetHeld = false
				m.cancel = nil
				m.mu.Unlock()
				return
			}
			m.mu.Unlock()
		}
	}()
}

func (m *extenderToggle) budgetWithheld() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.device != nil && m.enabled && m.budgetHeld
}
