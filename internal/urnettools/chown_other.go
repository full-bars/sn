//go:build !unix

package urnettools

// chownFdLikeStateOwner is a no-op on non-unix platforms.
func chownFdLikeStateOwner(stateDir string, fd int) error {
	return nil
}
