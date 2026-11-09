package urnettools

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// bindCapabilityBlocked reports an explicitly excluded CAP_NET_BIND_SERVICE.
// Missing or unreadable proc status retains the previous best-effort behavior.
func bindCapabilityBlocked(status []byte) bool {
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "CapBnd:" {
			mask, err := strconv.ParseUint(fields[1], 16, 64)
			return err == nil && mask&(1<<10) == 0
		}
	}
	return false
}

// installProviderBinary restores the Linux extender capability before restart.
// Tool self-updates use installBinary directly and do not need this capability.
func installProviderBinary(src, dst, user string) error {
	return installProviderBinaryWithConfig(src, dst, user, runtime.GOOS, NewSystemPlatformConfigurer(runtime.GOOS), os.Stderr)
}

func installProviderBinaryWithConfig(src, dst, user, platform string, cfg PlatformConfigurer, warnings io.Writer) error {
	return installProviderBinaryWithConfigAndStatus(src, dst, user, platform, cfg, warnings, "/proc/self/status")
}

func installProviderBinaryWithConfigAndStatus(src, dst, user, platform string, cfg PlatformConfigurer, warnings io.Writer, statusPath string) error {
	hadCapability := false
	if platform == "linux" {
		configured, _, err := cfg.CheckRequirement(dst, user)
		hadCapability = err == nil && configured
	}
	if err := installBinary(src, dst, user); err != nil {
		return err
	}
	if platform != "linux" {
		return nil
	}
	quoted := "'" + strings.ReplaceAll(dst, "'", "'\\''") + "'"
	if !hadCapability {
		fmt.Fprintf(warnings, "WARNING: previous extender capability absent or unverified; to enable it run: sudo setcap cap_net_bind_service=+ep %s\n", quoted)
		return nil
	}
	if data, err := os.ReadFile(statusPath); err == nil && bindCapabilityBlocked(data) {
		fmt.Fprintf(warnings, "WARNING: extender capability blocked by the process bounding set; after fixing that, run: sudo setcap cap_net_bind_service=+ep %s\n", quoted)
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
