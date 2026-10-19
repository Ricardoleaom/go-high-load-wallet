// Package jwttest stands in for the identity provider in tests: it serves a
// JWKS over HTTP and mints RS256 tokens carrying the claims the API reads.
package jwttest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Audience is the aud every token carries, as the realm's audience mapper
// adds it for the wallet API.
const Audience = "wallet-api"

type Issuer struct {
	server *httptest.Server

	mu  sync.RWMutex
	key *rsa.PrivateKey
	kid string
}

// NewIssuer starts a JWKS server that lives until the test ends.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	issuer := &Issuer{}
	issuer.Rotate(t)
	issuer.server = httptest.NewServer(issuer)
	t.Cleanup(issuer.server.Close)
	return issuer
}

// URL is the issuer identifier tokens carry in iss.
func (i *Issuer) URL() string {
	return i.server.URL + "/realms/test"
}

func (i *Issuer) JWKSURL() string {
	return i.server.URL + "/protocol/openid-connect/certs"
}

// Rotate replaces the signing key. From then on the JWKS publishes only
// the new key, which is how an IdP revokes the old one.
func (i *Issuer) Rotate(t testing.TB) {
	t.Helper()
	key := generateKey(t)

	i.mu.Lock()
	defer i.mu.Unlock()
	i.key = key
	i.kid = uuid.NewString()
}

// ServeHTTP serves the JWKS, so tests can wrap it (to simulate an IdP that
// is still starting, for instance).
func (i *Issuer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	i.mu.RLock()
	public, kid := i.key.PublicKey, i.kid
	i.mu.RUnlock()

	body := map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
	}}}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (i *Issuer) ProviderToken(t testing.TB, providerID string) string {
	t.Helper()
	return i.sign(t, jwt.MapClaims{"provider_id": providerID}, time.Now().Add(5*time.Minute))
}

// ProviderTokenWith is a valid provider token with override applied last:
// a value replaces the claim and nil removes it ({"exp": nil} for a token
// that never expires).
func (i *Issuer) ProviderTokenWith(t testing.TB, providerID string, override jwt.MapClaims) string {
	t.Helper()
	claims := i.standardClaims(jwt.MapClaims{"provider_id": providerID}, time.Now().Add(5*time.Minute))
	for name, value := range override {
		if value == nil {
			delete(claims, name)
			continue
		}
		claims[name] = value
	}

	i.mu.RLock()
	key, kid := i.key, i.kid
	i.mu.RUnlock()
	return signClaims(t, jwt.SigningMethodRS256, key, kid, claims)
}

func (i *Issuer) InternalToken(t testing.TB) string {
	t.Helper()
	realmAccess := map[string]any{"roles": []string{"internal"}}
	return i.sign(t, jwt.MapClaims{"realm_access": realmAccess}, time.Now().Add(5*time.Minute))
}

// OperatorToken is what the console gets for a person signed in with the
// operator realm role: a user's token, with no provider_id.
func (i *Issuer) OperatorToken(t testing.TB) string {
	t.Helper()
	realmAccess := map[string]any{"roles": []string{"operator"}}
	return i.sign(t, jwt.MapClaims{"realm_access": realmAccess, "preferred_username": "operator"}, time.Now().Add(5*time.Minute))
}

func (i *Issuer) ExpiredProviderToken(t testing.TB, providerID string) string {
	t.Helper()
	return i.sign(t, jwt.MapClaims{"provider_id": providerID}, time.Now().Add(-time.Minute))
}

// HS256ProviderToken is signed with a shared secret instead of the RSA
// key: the shape of an algorithm-confusion attack the API must refuse.
func (i *Issuer) HS256ProviderToken(t testing.TB, providerID string) string {
	t.Helper()
	i.mu.RLock()
	kid := i.kid
	i.mu.RUnlock()

	return signClaims(t, jwt.SigningMethodHS256, []byte("not-the-rsa-key"), kid,
		i.standardClaims(jwt.MapClaims{"provider_id": providerID}, time.Now().Add(5*time.Minute)))
}

// UnknownKeyProviderToken is signed by a key the JWKS never published.
func (i *Issuer) UnknownKeyProviderToken(t testing.TB, providerID string) string {
	t.Helper()
	return signClaims(t, jwt.SigningMethodRS256, generateKey(t), uuid.NewString(),
		i.standardClaims(jwt.MapClaims{"provider_id": providerID}, time.Now().Add(5*time.Minute)))
}

func (i *Issuer) sign(t testing.TB, claims jwt.MapClaims, expiresAt time.Time) string {
	t.Helper()
	i.mu.RLock()
	key, kid := i.key, i.kid
	i.mu.RUnlock()

	return signClaims(t, jwt.SigningMethodRS256, key, kid, i.standardClaims(claims, expiresAt))
}

// standardClaims adds what every token from this issuer carries: issuer,
// audience, issue time and expiry.
func (i *Issuer) standardClaims(claims jwt.MapClaims, expiresAt time.Time) jwt.MapClaims {
	claims["iss"] = i.URL()
	claims["aud"] = Audience
	claims["iat"] = time.Now().Unix()
	claims["exp"] = expiresAt.Unix()
	return claims
}

func signClaims(t testing.TB, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = kid

	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return signed
}

func generateKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return key
}
