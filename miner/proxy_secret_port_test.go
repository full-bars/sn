package miner

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestProxySecretURLCauseAndPrivateConfig(t *testing.T) {
	const secret = "canary-token-5518"
	for _, err := range []error{stripURLFromError(errors.New(secret)), (&proxyURLParseError{err: errors.New(secret)})} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("error contains canary")
		}
	}
	withTempHome(t)
	path, err := providerStatePath("proxy")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0700); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"example.com:1080": "canary-pass-9042"}})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("proxy config mode = %o", info.Mode().Perm())
	}
}
