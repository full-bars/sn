package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// createFakeJWTWithClaims builds a minimal JWT with the given claims.
func createFakeJWTWithClaims(claims map[string]interface{}) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return fmt.Sprintf("%s.%s.fakesig", header, payload)
}

// setTestHome sets up a temp home directory with HOME/USERPROFILE env vars
// and a fake account JWT. Used by renewal watcher tests.
func setTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	accountJwt := createFakeJWTWithClaims(map[string]interface{}{
		"network_id": "net-1",
		"exp":        float64(time.Now().Add(48 * time.Hour).Unix()),
	})
	urnetworkDir := filepath.Join(dir, ".urnetwork")
	if err := os.MkdirAll(urnetworkDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(urnetworkDir, "jwt"), []byte(accountJwt), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// setRenewalTestHome sets up a temp home directory with a JWT for renewal tests.
func setRenewalTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	accountJwt := createFakeJWTWithClaims(map[string]interface{}{
		"network_id": "net-1",
		"exp":        float64(time.Now().Add(48 * time.Hour).Unix()),
	})
	urnetworkDir := filepath.Join(dir, ".urnetwork")
	if err := os.MkdirAll(urnetworkDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(urnetworkDir, "jwt"), []byte(accountJwt), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}
