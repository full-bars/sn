package main

import "github.com/urfoundation/sn/provider"

// Version is set at link time via -ldflags "-X main.Version=...".
// We forward it to provider.Version so RequireVersion() returns it
// instead of the "dev" fallback.
var Version string

func main() {
	provider.Version = Version
	provider.Main()
}
