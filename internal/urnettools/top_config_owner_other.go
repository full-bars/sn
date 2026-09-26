//go:build !unix

package urnettools

import "os"

// chownConfigFdToDirOwner is a no-op on platforms without a unix ownership
// model (the config dir belongs to the invoking user on Windows already).
func chownConfigFdToDirOwner(file *os.File, dir string) {}

// chownConfigDirToHomeOwner is a no-op off unix: ownership transfer is not a
// concept there.
func chownConfigDirToHomeOwner(dir string) {}
