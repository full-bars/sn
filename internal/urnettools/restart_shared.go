package urnettools

import (
	"fmt"
	"strings"
)

// refuseRestartWhileProfileShared returns an error, before anything is
// stopped, when a provider process other than the target is running. The
// stop half of an unowned restart talks to provider.sock in the state dir,
// and with two providers on one profile the socket may belong to the other
// one: the restart would shut that provider down and start a duplicate of the
// target. It fails closed when the process list cannot be read.
func refuseRestartWhileProfileShared(p Provider) error {
	label := providerLabel(p)
	live, err := runningProviderProcessesFn()
	if err != nil {
		return fmt.Errorf("could not restart provider %s: could not list running provider processes to check that no other provider shares its state dir: %v\nnothing was stopped; check the process list and try again", label, err)
	}
	var others []string
	for _, other := range live {
		if other.PID <= 0 || (p.PID > 0 && other.PID == p.PID) {
			continue
		}
		others = append(others, fmt.Sprintf("pid %d (%s)", other.PID, other.Binary))
	}
	if len(others) == 0 {
		return nil
	}
	return fmt.Errorf("could not restart provider %s: another provider process is running and may use the same state dir (%s):\n  %s\nthe control socket may belong to that process, so this could stop the wrong provider; nothing was stopped. End the extra process in Task Manager, then run: %s restart",
		label, p.StateDir, strings.Join(others, "\n  "), programName())
}
