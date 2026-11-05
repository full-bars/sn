package urnettools

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderStateOnlyCleanSucceeds(t *testing.T) {
	cases := []struct {
		name, action, raw string
		wantErr           bool
	}{
		{"clean verify", "verify", `{"status":"clean","store":"clean","schema":"supported"}`, false},
		{"unchecked verify", "verify", `{"status":"unchecked","store":"clean","schema":"supported"}`, true},
		{"locked verify", "verify", `{"status":"locked"}`, true},
		{"empty status is not clean", "verify", `{}`, true},
		{"missing status identities", "identities", `{"slots":[]}`, true},
		{"clean identities", "identities", `{"status":"clean","slots":[{"slot":"slot-1a2b3c4d","outcome":"adopted","events":2}]}`, false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		err := renderState(c.action, c.raw, &out)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
		if c.wantErr && strings.Contains(out.String(), "state: clean") {
			t.Errorf("%s: printed clean: %s", c.name, out.String())
		}
	}
	if err := renderState("verify", "not json", &bytes.Buffer{}); err == nil {
		t.Error("an unreadable reply must be an error")
	}
}

func TestCmdStateRejectsBadUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"repair"}} {
		if err := cmdState(args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: %v", args, err)
		}
	}
}
