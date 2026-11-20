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
	mu              sync.Mutex
	device          extenderToggleDevice
	allow           func() bool
	cancel          context.CancelFunc
	generation      uint64
	settingKnown    bool
	enabled         bool
	budgetHeld      bool
	appliedCarriers extenderCarrierConfig
	carrierKnown    bool
	restartRole     func() error
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
	m.carrierKnown = true
	m.appliedCarriers = extenderCarrierConfigFromState(globalControlState)
	m.reconcileLocked(extenderSettingEnabled(globalControlState))
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.device == device {
			m.stopCheckLocked()
			m.device = nil
			m.allow = nil
			m.restartRole = nil
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
		m.appliedCarriers = extenderCarrierConfigFromState(globalControlState)
		m.carrierKnown = true
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
				m.appliedCarriers = extenderCarrierConfigFromState(globalControlState)
				m.carrierKnown = true
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

type extenderCarrierConfig struct {
	udp443  bool
	udp4053 bool
	udp53   bool
}

func extenderCarrierConfigFromState(state *controlState) extenderCarrierConfig {
	cfg := extenderCarrierConfig{
		udp443:  true,
		udp4053: true,
		udp53:   true,
	}
	if state == nil {
		return cfg
	}
	if v, ok := state.get("extender_udp443"); ok && strings.EqualFold(v, "off") {
		cfg.udp443 = false
	}
	if v, ok := state.get("extender_udp4053"); ok && strings.EqualFold(v, "off") {
		cfg.udp4053 = false
	}
	if v, ok := state.get("extender_udp53"); ok && strings.EqualFold(v, "off") {
		cfg.udp53 = false
	}
	return cfg
}

func (m *extenderToggle) RestartCarriers() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restartCarriersLocked(false)
}

func (m *extenderToggle) restartCarriersIfChanged() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restartCarriersLocked(true)
}

func (m *extenderToggle) restartCarriersLocked(onlyIfChanged bool) error {
	current := extenderCarrierConfigFromState(globalControlState)
	if m.device == nil || !m.enabled || m.budgetHeld {
		m.appliedCarriers = current
		m.carrierKnown = true
		return nil
	}
	if onlyIfChanged && m.carrierKnown && m.appliedCarriers == current {
		return nil
	}
	var err error
	if m.restartRole != nil {
		err = m.restartRole()
	} else {
		m.device.SetProvideExtender(false)
		m.device.SetProvideExtender(true)
	}
	if err != nil {
		return err
	}
	m.appliedCarriers = current
	m.carrierKnown = true
	return nil
}
