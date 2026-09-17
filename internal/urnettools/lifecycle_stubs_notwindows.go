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
