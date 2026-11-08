package urnettools

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestFirewalldRuntimeAndPermanentWithoutReload(t *testing.T) {
	var writes [][]string
	fm := &FirewallManager{Platform: "linux", LookPath: func(s string) (string, error) {
		if s == "firewall-cmd" {
			return s, nil
		}
		return "", errors.New("missing")
	}, ExecCmd: func(_ string, args ...string) ([]byte, error) {
		if args[0] == "--state" {
			return []byte("running"), nil
		}
		if args[0] == "--get-default-zone" {
			return []byte("public"), nil
		}
		for _, a := range args {
			if strings.HasPrefix(a, "--query-port=") {
				if a == "--query-port=443/tcp" && args[0] != "--permanent" {
					return []byte("yes"), nil
				}
				return []byte("no"), errors.New("not enabled")
			}
		}
		writes = append(writes, append([]string(nil), args...))
		return nil, nil
	}}
	st := fm.Detect()
	want := [][]string{{"--permanent", "--zone=public", "--add-port=443/tcp"}, {"--zone=public", "--add-port=443/udp"}, {"--permanent", "--zone=public", "--add-port=443/udp"}, {"--zone=public", "--add-port=4053/udp"}, {"--permanent", "--zone=public", "--add-port=4053/udp"}}
	if st.PortsOpen {
		t.Fatal("permanent rule missing")
	}
	ok, err := fm.Apply(st)
	if !ok || err != nil || !reflect.DeepEqual(writes, want) {
		t.Fatalf("applied=%v err=%v writes=%v", ok, err, writes)
	}
}

func TestFirewalldUnknownFailsClosed(t *testing.T) {
	writes := 0
	fm := &FirewallManager{Platform: "linux", LookPath: func(s string) (string, error) {
		if s == "firewall-cmd" {
			return s, nil
		}
		return "", errors.New("missing")
	}, ExecCmd: func(_ string, args ...string) ([]byte, error) {
		if args[0] == "--state" {
			return []byte("running"), nil
		}
		if args[0] == "--get-default-zone" {
			return nil, errors.New("denied")
		}
		writes++
		return nil, nil
	}}
	st := fm.Detect()
	ok, err := fm.Apply(st)
	if st.Kind != FirewallFirewalld || ok || err == nil || writes != 0 {
		t.Fatalf("status=%+v applied=%v err=%v writes=%d", st, ok, err, writes)
	}
	ok, err = fm.Apply(FirewallStatus{Kind: FirewallFirewalld})
	if ok || err == nil || writes != 0 {
		t.Fatalf("empty plan applied=%v err=%v writes=%d", ok, err, writes)
	}
}
