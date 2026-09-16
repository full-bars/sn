package provider

import (
	"errors"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// ErrTokenInvalid is returned when a token is invalid or expired.
var ErrTokenInvalid = errors.New("auth: token is invalid or expired")

// validateJWTExpiry parses the JWT locally to check the 'exp' claim.
// It returns ErrTokenInvalid if the token is definitely expired (with 30s leeway).
func validateJWTExpiry(byJwt string) error {
	expParser := gojwt.NewParser()
	if tok, _, parseErr := expParser.ParseUnverified(byJwt, gojwt.MapClaims{}); parseErr == nil {
		if claims, ok := tok.Claims.(gojwt.MapClaims); ok {
			if exp, ok := claims["exp"].(float64); ok && time.Now().Unix() > int64(exp)+30 {
				return ErrTokenInvalid
			}
		}
	}
	return nil
}
