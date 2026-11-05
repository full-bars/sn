package urnettools

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A rule that exists but is disabled (or blocks, or is outbound) is not a
// configured extender rule: detection must report it open=false and apply must
// delete and re-add it rather than count the name as present.
func TestExtenderWindowsDisabledRuleIsRepaired(t *testing.T) {
	show := func(name, enabled, action string) []byte {
		return []byte("Rule Name:        " + name + "\n----\nEnabled:          " + enabled + "\nDirection:        In\nAction:           " + action + "\n")
	}
	state := map[string][2]string{
		"URnetwork Extender":     {"Yes", "Allow"},
		"URnetwork Extender UDP": {"No", "Allow"},
	}
	var calls []string
	run := func(_ string, args ...string) ([]byte, error) {
		name := strings.TrimPrefix(args[4], "name=")
		calls = append(calls, args[2]+" "+name)
		switch args[2] {
		case "show":
			st, ok := state[name]
			if !ok {
				return nil, errors.New("no rules match")
			}
			return show(name, st[0], st[1]), nil
		case "delete":
			delete(state, name)
		case "add":
			state[name] = [2]string{"Yes", "Allow"}
		}
		return nil, nil
	}
	fm := &FirewallManager{Platform: "windows", ExecCmd: run}
	if fm.Detect().PortsOpen {
		t.Fatal("a disabled UDP rule was reported open")
	}
	if ok, _, _ := (&SystemPlatformConfigurer{ExecCmd: run}).checkWindows(); ok {
		t.Fatal("platform check accepted a disabled UDP rule")
	}
	calls = nil
	applied, err := fm.Apply(fm.Detect())
	if err != nil || !applied {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	var mutating []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "show ") {
			mutating = append(mutating, c)
		}
	}
	if !reflect.DeepEqual(mutating, []string{"delete URnetwork Extender UDP", "add URnetwork Extender UDP"}) {
		t.Fatalf("mutations = %v", mutating)
	}
	if !fm.Detect().PortsOpen {
		t.Fatal("repaired rules not reported open")
	}
}
