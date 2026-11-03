//go:build linux || darwin

package miner

import (
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urfoundation/sn/clientauth"
)

// The production draw takes a token's point from its id, and from its
// signature or its whole text when it has no id: the fallbacks are what a
// token without a `jti` relies on, so each one is pinned here rather than left
// to the id-only tests in network_token_renewal_window_test.go.

// A signed network token with no `jti`.
func renewalTestJwtSignedWithoutId(t *testing.T, key []byte, issued time.Time, expires time.Time) string {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{
		"network_id": renewalTestNetworkId,
		"user_id":    renewalTestUserId,
		"iat":        issued.Unix(),
		"exp":        expires.Unix(),
	}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestNetworkTokenRenewalDrawWithoutId(t *testing.T) {
	now := renewalTestBase
	day := 24 * time.Hour
	key := []byte("renewal-test-key")

	signed := renewalTestJwtSignedWithoutId(t, key, now, now.Add(30*day))
	draw := networkTokenRenewalDraw(signed)
	if draw != networkTokenRenewalDraw(signed) {
		t.Fatal("one token without an id took two points")
	}
	if draw < 0 || 1 <= draw {
		t.Fatalf("draw %v is outside [0, 1)", draw)
	}
	// the signature is the seed, so another issue time changes the token and
	// its point, while the same token keeps its point
	if networkTokenRenewalDraw(renewalTestJwtSignedWithoutId(t, key, now.Add(time.Hour), now.Add(31*day))) == draw {
		t.Fatal("two tokens without ids and with different signatures took the same point")
	}

	// an unsigned token has no signature either: the whole text is the seed
	unsigned := renewalTestJwt(t, now, now.Add(30*day), "no-id")
	if networkTokenRenewalDraw(unsigned) != networkTokenRenewalDraw(unsigned) {
		t.Fatal("an unsigned token took two points")
	}
	if networkTokenRenewalDraw("network-jwt") != networkTokenRenewalDraw("network-jwt") {
		t.Fatal("text that is not a JWT took two points")
	}
	if got := networkTokenRenewalDraw("network-jwt"); got < 0 || 1 <= got {
		t.Fatalf("draw %v of text that is not a JWT is outside [0, 1)", got)
	}
	if networkTokenRenewalDraw("network-jwt") == networkTokenRenewalDraw("network-jwt-other") {
		t.Fatal("two different texts took the same point")
	}
}

// The id wins over the signature: one id signed differently is one point.
func TestNetworkTokenRenewalDrawPrefersTheId(t *testing.T) {
	now := renewalTestBase
	day := 24 * time.Hour
	claims := func() gojwt.MapClaims {
		return gojwt.MapClaims{
			"network_id": renewalTestNetworkId,
			"user_id":    renewalTestUserId,
			"iat":        now.Unix(),
			"exp":        now.Add(30 * day).Unix(),
			"jti":        "one-id",
		}
	}
	first, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, claims()).SignedString([]byte("key-one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, claims()).SignedString([]byte("key-two"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("the two signed tokens are identical")
	}
	if networkTokenRenewalDraw(first) != networkTokenRenewalDraw(second) {
		t.Fatal("tokens with one id and different signatures took different points")
	}
}

// A sign-in that replaces the token file starts a new schedule: a token
// already past its point renews at once even a minute after the previous
// renewal, while the same token is still held by the floor.
func TestNetworkTokenRenewerNewSignInResetsTheFloor(t *testing.T) {
	day := 24 * time.Hour
	expired := renewalTestJwt(t, renewalTestBase.Add(-40*day), renewalTestBase.Add(-10*day), "expired")
	fixture := newRenewalTestFixture(t, expired)
	fixture.drawWindow(t, 0.5, 0.5, 0.5, 0.5)
	floor := fixture.renewer.settings.RenewalFloor

	// a renewal that answers a token already past its point: the new token is
	// due at once, and only the floor holds it
	pastItsPoint := renewalTestJwt(t, renewalTestBase.Add(-16*day), renewalTestBase.Add(14*day), "past its point")
	fixture.refresh.answer = renewalTestAnswer(pastItsPoint)
	if wait := fixture.renewer.pass(t.Context()); wait != floor {
		t.Fatalf("first renewal waits %s, want the floor %s", wait, floor)
	}
	fixture.event(t)
	if fixture.token(t) != pastItsPoint {
		t.Fatal("the first renewal did not land")
	}

	// the same token, inside the floor: held
	fixture.clock.now = renewalTestBase.Add(time.Minute)
	if wait := fixture.renewer.pass(t.Context()); wait != floor-time.Minute {
		t.Fatalf("inside the floor the wait is %s, want %s", wait, floor-time.Minute)
	}
	if fixture.refresh.count() != 1 {
		t.Fatalf("renewed %d times inside the floor", fixture.refresh.count())
	}

	// a new sign-in that is also already past its point: renewed at once,
	// because the token changed
	signIn := renewalTestJwt(t, fixture.clock.now.Add(-16*day), fixture.clock.now.Add(14*day), "new sign-in")
	if err := clientauth.WriteNetworkToken(fixture.path, signIn); err != nil {
		t.Fatal(err)
	}
	fixture.refresh.answer = renewalTestAnswer(renewalTestJwt(t, fixture.clock.now, fixture.clock.now.Add(30*day), "renewed"))
	if wait := fixture.renewer.pass(t.Context()); wait != fixture.renewer.settings.MaximumWait {
		t.Fatalf("a new sign-in past its point waits %s, want it renewed at once", wait)
	}
	fixture.event(t)
	if fixture.refresh.count() != 2 {
		t.Fatalf("renewals %d, want 2", fixture.refresh.count())
	}
}
