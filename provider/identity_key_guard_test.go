package provider

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The proxy stores are keyed by proxy IDENTITY (ProxySettings.Key(): address
// alone for an unauthenticated proxy, address plus user for an authenticated
// one), so two accounts sharing one gateway address stay two proxies. A call
// that passes proxySettings.Address to one of them looks right and passes every
// test that uses an unauthenticated proxy, where the two are identical, yet
// files an authenticated proxy under a key nothing else ever reads. Two such
// calls arrived in a merge of two branches written against different keying,
// so this pins the rule by reading the sources.
func TestIdentityKeyedStoresAreNeverGivenABareAddress(t *testing.T) {
	stores := regexp.MustCompile(
		`(globalProxyFailureHistory\.\w+|globalProxySlowRetryState\.Load\(\)\.\w+|globalProvenProxies\.\w+|getProxyIndex)\(\s*(proxySettings|settings|s|ps)\.Address\b`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(source), "\n") {
			if stores.MatchString(line) {
				t.Errorf("%s:%d passes a bare address to an identity-keyed store, use .Key(): %s",
					file, number+1, strings.TrimSpace(line))
			}
		}
	}
}
