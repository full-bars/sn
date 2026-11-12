package urnettools

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOpenBSDCheckScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urnetwork")
	binary := "/usr/local/bin/urnetwork"
	if err := openbsdCheckScript(path, binary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing script: %v", err)
	}
	if err := os.WriteFile(path, []byte(`daemon="`+binary+` provide"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := openbsdCheckScript(path, binary); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"", binary + "-other"} {
		if err := openbsdCheckScript(path, other); err == nil {
			t.Errorf("accepted mismatched binary %q", other)
		}
	}
}

func TestOpenBSDScriptServesBinary(t *testing.T) {
	binary := "/usr/local/libexec/urnetwork-provider/bin/urnetwork"
	for _, tt := range []struct {
		script string
		want   bool
	}{
		{`daemon="` + binary + ` provide"`, true},
		{`# daemon="` + binary + ` provide"`, false},
		{`daemon="` + binary + `-other provide"`, false},
		{`daemon="` + binary + ` provide -x"`, false},
	} {
		if got := openbsdScriptServesBinary(tt.script, binary); got != tt.want {
			t.Errorf("openbsdScriptServesBinary(%q) = %v, want %v", tt.script, got, tt.want)
		}
	}
}

func TestOpenBSDRcctlArgv(t *testing.T) {
	tests := []struct {
		name       string
		euid       int
		doas, sudo bool
		want       string
		args       []string
		wantErr    bool
	}{
		{"root", 0, false, false, "rcctl", []string{"restart", "urnetwork"}, false},
		{"prefer doas", 1000, true, true, "doas", []string{"rcctl", "restart", "urnetwork"}, false},
		{"sudo fallback", 1000, false, true, "sudo", []string{"rcctl", "restart", "urnetwork"}, false},
		{"no escalation", 1000, false, false, "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, args, err := openbsdRcctlArgv(tt.euid, tt.doas, tt.sudo, "restart", "urnetwork")
			if (err != nil) != tt.wantErr || name != tt.want || !reflect.DeepEqual(args, tt.args) {
				t.Fatalf("got (%q, %v, %v), want (%q, %v, error=%v)", name, args, err, tt.want, tt.args, tt.wantErr)
			}
		})
	}
}

func TestOptimizeForOpenBSDRefuses(t *testing.T) {
	err := optimizeFor("openbsd")()
	if err == nil {
		t.Fatal("optimizeFor(openbsd) succeeded; it must refuse instead of writing Linux sysctl files")
	}
}
