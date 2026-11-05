package urnettools

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// installProviderBinary restores the Linux extender capability before restart.
// Tool self-updates use installBinary directly and do not need this capability.
func installProviderBinary(src, dst, user string) error {
	return installProviderBinaryWithConfig(src, dst, user, runtime.GOOS, NewSystemPlatformConfigurer(runtime.GOOS), os.Stderr)
}

func installProviderBinaryWithConfig(src, dst, user, platform string, cfg PlatformConfigurer, warnings io.Writer) error {
	if err := installBinary(src, dst, user); err != nil {
		return err
	}
	if platform != "linux" {
		return nil
	}
	configured, _, err := cfg.CheckRequirement(dst, user)
	if err == nil && configured {
		return nil
	}
	if err == nil {
		_, _, err = cfg.ApplyRequirement(dst, user)
	}
	if err != nil {
		// Single quotes preserve spaces and shell metacharacters in the exact path.
		quoted := "'" + strings.ReplaceAll(dst, "'", "'\\''") + "'"
		msg := strings.Join(strings.Fields(err.Error()), " ")
		fmt.Fprintf(warnings, "WARNING: extender capability not verified for %q: %s; run: sudo setcap cap_net_bind_service=+ep %s\n", dst, msg, quoted)
	}
	return nil
}
