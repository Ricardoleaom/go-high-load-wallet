package jwttest_test

import (
	"context"
	"testing"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/jwttest"
)

func TestIssuedTokensVerifyAgainstTheServedJWKS(t *testing.T) {
	issuer := jwttest.NewIssuer(t)
	keys := newKeyfunc(t, issuer.JWKSURL())

	var claims struct {
		jwt.RegisteredClaims
		ProviderID string `json:"provider_id"`
	}
	_, err := jwt.ParseWithClaims(
		issuer.ProviderToken(t, "provider-a"), &claims, keys.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
	)
	if err != nil {
		t.Fatalf("provider token rejected: %v", err)
	}
	if claims.ProviderID != "provider-a" {
		t.Fatalf("provider_id = %q, want provider-a", claims.ProviderID)
	}

	if _, err := jwt.Parse(issuer.InternalToken(t), keys.Keyfunc, jwt.WithValidMethods([]string{"RS256"})); err != nil {
		t.Fatalf("internal token rejected: %v", err)
	}
}

func TestExpiredAndForeignTokensAreRejected(t *testing.T) {
	issuer := jwttest.NewIssuer(t)
	keys := newKeyfunc(t, issuer.JWKSURL())

	tokens := map[string]string{
		"expired":     issuer.ExpiredProviderToken(t, "provider-a"),
		"hs256":       issuer.HS256ProviderToken(t, "provider-a"),
		"unknown key": issuer.UnknownKeyProviderToken(t, "provider-a"),
	}
	for name, token := range tokens {
		if _, err := jwt.Parse(token, keys.Keyfunc, jwt.WithValidMethods([]string{"RS256"})); err == nil {
			t.Errorf("%s token was accepted", name)
		}
	}
}

func newKeyfunc(t *testing.T, url string) keyfunc.Keyfunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	keys, err := keyfunc.NewDefaultCtx(ctx, []string{url})
	if err != nil {
		t.Fatalf("load JWKS: %v", err)
	}
	return keys
}
