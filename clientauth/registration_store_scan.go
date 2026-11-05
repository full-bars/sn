package clientauth

import "errors"

// errScanStopped ends a scanNames visit early without reporting a failure.
// Every platform's registrationStore shares it, so callers can stop a scan
// the same way whether the platform scans a directory or refuses custody.
var errScanStopped = errors.New("registration directory scan stopped")
