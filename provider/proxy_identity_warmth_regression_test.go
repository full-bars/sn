package provider

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func jwtRegressionSettings(address, user string) *connect.ProxySettings {
	return &connect.ProxySettings{
		Network: "tcp",
		Address: address,
		Auth:    &proxy.Auth{User: user, Password: "p"},
	}
}

func TestPrioritizeAndScheduleProxies_JWTWarmthIsPerIdentity(t *testing.T) {
	t.Setenv("URNETWORK_HOT_RESTART", "1")
	restore := withGlobalStore(t, t.TempDir()+"/jwts.json")
	defer restore()

	validJWT := createFakeJWTWithClaims(map[string]interface{}{
		"client_id":  testClientId,
		"exp":        float64(time.Now().Add(time.Hour).Unix()),
		"network_id": "net-main",
	})
	warm := jwtRegressionSettings("gw.example:1080", "u1")
	cold := jwtRegressionSettings("gw.example:1080", "u2")
	if err := loadGlobalClientJWTStore().Put(jwtStoreKey(warm), clientJWTEntry{
		ByClientJWT: validJWT,
		ClientID:    testClientId,
		NetworkID:   "net-main",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	_, warmCount, renewableCount, coldCount := prioritizeAndScheduleProxies(
		[]*connect.ProxySettings{warm, cold}, map[string]string{}, "net-main",
	)
	if warmCount != 1 || renewableCount != 0 || coldCount != 1 {
		t.Fatalf("warmth counts = warm %d renewable %d cold %d, want 1/0/1", warmCount, renewableCount, coldCount)
	}
}
