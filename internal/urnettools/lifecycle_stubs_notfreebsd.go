//go:build !freebsd

package urnettools

import "errors"

// installServiceFreeBSD is implemented in lifecycle_freebsd.go. cmdInstallService
// is compiled on every platform and dispatches on runtime.GOOS, so the symbol
// must exist for every build.
func installServiceFreeBSD(_, _ string, _ bool) error {
	return errors.New("rc.d service installation is only available on FreeBSD")
}
