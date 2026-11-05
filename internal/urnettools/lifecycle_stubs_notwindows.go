//go:build !windows

package urnettools

import "errors"

func cmdStartWindows(_ Provider, _, _ bool) error {
	return errors.New("not supported on this platform")
}

func cmdStopWindows(_ Provider, _, _ bool) error {
	return errors.New("not supported on this platform")
}

func cmdRestartWindows(_ Provider, _, _ bool) error {
	return errors.New("not supported on this platform")
}

func restartProviderWindows(_ Provider) error {
	return errors.New("not supported on this platform")
}

// stopProviderService stops the per-user supervisor that owns the provider.
// Only Windows has one outside systemd/rc.d/OpenRC, which the unit handling
// already covers, so this is a no-op elsewhere.
func stopProviderService(_ Provider) error { return nil }

// Only Windows has a per-user supervisor to own a provider. Elsewhere the
// service seams are inert: everything is "owned", so nothing extra is stopped.
func providerOwnedByService(_ Provider) bool { return true }

func stopBareProvider(_ Provider) error { return nil }

func removeProviderServiceFiles() {}
