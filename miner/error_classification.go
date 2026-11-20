package miner

import (
	"errors"
	"os"
	"runtime"
	"syscall"
)

// error_classification.go provides the smallest local equivalent of the
// connect/extender ClassifyBindError for the miner package.
//
// The connect fork does not yet ship ClassifyBindError and the per-port error
// bookkeeping. This local implementation keeps sn's worker logic working and
// records the bind failure class per UDP port. The connect fork should absorb
// these constants and the function; this file is the follow-up and UNVERIFIED.

// BindFailureAddressInUse is the bind failure class for a port already held
// by another process.
const BindFailureAddressInUse = "address_in_use"

// BindFailurePermissionDenied is the bind failure class for a non-root process
// that cannot bind a privileged port without a capability or sysctl.
const BindFailurePermissionDenied = "permission_denied"

// BindFailureUnknown is the bind failure class for any other cause. The class
// is always recorded; the exact error text is kept at the caller's discretion.
const BindFailureUnknown = "unknown"

// ClassifyBindError names the cause of a listen failure from its error type,
// never from its text. It is used to turn the per-port error bookkeeping into
// the reason a carrier could not bind, which a later caller can show without
// looking up the raw error. On Windows the socket error is a syscall error
// whose number is the Windows value, so it is compared by value there.
func ClassifyBindError(err error) string {
	if err == nil {
		return ""
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && runtime.GOOS == "windows" {
		switch errno {
		case 10048: // WSAEADDRINUSE
			return BindFailureAddressInUse
		case 10013: // WSAEACCES
			return BindFailurePermissionDenied
		}
	}
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return BindFailureAddressInUse
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM), errors.Is(err, os.ErrPermission):
		return BindFailurePermissionDenied
	}
	return BindFailureUnknown
}
