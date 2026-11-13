//go:build !freebsd && !openbsd

package urnettools

import "errors"

func bsdServiceControl(_ Provider, _ string) error {
	return errors.New("rc.d service control is only available on FreeBSD and OpenBSD")
}
