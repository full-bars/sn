package urnettools

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func openbsdCleanupMissingScript(err error, removeCron func() error) (bool, error) {
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := removeCron(); err != nil {
		return true, fmt.Errorf("remove auto-update cron entry: %w", err)
	}
	return true, nil
}

func openbsdCheckScript(path, binary string) error {
	script, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no rc.d script: %w", err)
	}
	if binary == "" || !openbsdScriptServesBinary(string(script), binary) {
		return fmt.Errorf("script does not serve the selected binary")
	}
	return nil
}

// OpenBSD keeps third-party service scripts in /etc/rc.d.
const openbsdRcScriptDir = "/etc/rc.d"

func openbsdRcScriptPath(service string) string {
	return openbsdRcScriptDir + "/" + service
}

// openbsdScriptServesBinary recognizes only the installer's literal daemon line.
func openbsdScriptServesBinary(script, binary string) bool {
	want := `daemon="` + binary + ` provide"`
	for _, line := range strings.Split(script, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// openbsdRcctlArgv chooses the privilege escalation available to the caller.
func openbsdRcctlArgv(euid int, hasDoas, hasSudo bool, args ...string) (string, []string, error) {
	switch {
	case euid == 0:
		return "rcctl", args, nil
	case hasDoas:
		return "doas", append([]string{"rcctl"}, args...), nil
	case hasSudo:
		return "sudo", append([]string{"rcctl"}, args...), nil
	default:
		return "", nil, fmt.Errorf("`rcctl %s` needs root and neither doas nor sudo is installed - run it as root", strings.Join(args, " "))
	}
}
