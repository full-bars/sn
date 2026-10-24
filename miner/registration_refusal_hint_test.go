package miner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRegistrationRefusalHint(t *testing.T) {
	for _, reason := range []string{"provider_key_requires_explicit_recovery", "legacy_identity_requires_explicit_recovery"} {
		err := fmt.Errorf("%w: client registration requires recovery: %s", errProviderStartupRefused, reason)
		hint := registrationRefusalHint(err)
		for _, want := range []string{"--allow-client-registration", `"provider start"`, `"provider setup"`, "restore"} {
			if !strings.Contains(hint, want) {
				t.Errorf("%s: hint missing %q: %q", reason, want, hint)
			}
		}
	}
	for _, err := range []error{nil, errors.New("api unreachable"), fmt.Errorf("%w: port in use", errProviderStartupRefused)} {
		if hint := registrationRefusalHint(err); hint != "" {
			t.Errorf("unrelated failure %v got a hint: %q", err, hint)
		}
	}
}
