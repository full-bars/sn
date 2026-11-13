package urnettools

import (
	"fmt"
	"strconv"
	"strings"
)

// openbsdManagedPID returns zero for no match and -1 for an unknown or
// ambiguous result. Only a positive PID identifies a managed process.
func openbsdManagedPID(runfile string, lookup func(string, string) ([]byte, error)) int {
	values := make(map[string]string)
	for _, line := range strings.Split(runfile, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "pexp" && key != "daemon_rtable") {
			continue
		}
		if _, exists := values[key]; exists {
			return -1
		}
		values[key] = value
	}
	pattern, table := values["pexp"], values["daemon_rtable"]
	if pattern == "" || strings.ContainsAny(pattern, "\r\n") || table == "" {
		return -1
	}
	if _, err := strconv.ParseUint(table, 10, 31); err != nil {
		return -1
	}
	out, err := lookup(table, pattern)
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0
	}
	if len(fields) != 1 {
		return -1
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return -1
	}
	return pid
}

func openbsdCleanupNeedsStop(p Provider, pid int) (bool, error) {
	if pid == 0 {
		return false, nil
	}
	if pid < 0 || p.PID <= 0 || pid != p.PID {
		return false, fmt.Errorf("%s is not the uniquely matched rc.d-managed provider; refusing service action", providerLabel(p))
	}
	return true, nil
}
