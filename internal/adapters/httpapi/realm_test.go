package httpapi_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// realm is the part of deploy/keycloak/realm-export.json the API and the
// console rely on.
type realm struct {
	Roles struct {
		Realm []struct {
			Name string `json:"name"`
		} `json:"realm"`
	} `json:"roles"`
	Clients []realmClient `json:"clients"`
	Users   []realmUser   `json:"users"`
}

type realmClient struct {
	ClientID                  string            `json:"clientId"`
	PublicClient              bool              `json:"publicClient"`
	StandardFlowEnabled       bool              `json:"standardFlowEnabled"`
	DirectAccessGrantsEnabled bool              `json:"directAccessGrantsEnabled"`
	ServiceAccountsEnabled    bool              `json:"serviceAccountsEnabled"`
	ImplicitFlowEnabled       bool              `json:"implicitFlowEnabled"`
	RedirectURIs              []string          `json:"redirectUris"`
	WebOrigins                []string          `json:"webOrigins"`
	Attributes                map[string]string `json:"attributes"`
	ProtocolMappers           []realmMapper     `json:"protocolMappers"`
}

type realmMapper struct {
	ProtocolMapper string            `json:"protocolMapper"`
	Config         map[string]string `json:"config"`
}

type realmUser struct {
	Username   string   `json:"username"`
	RealmRoles []string `json:"realmRoles"`
}

func loadRealm(t *testing.T) realm {
	t.Helper()
	raw, err := os.ReadFile("../../../deploy/keycloak/realm-export.json")
	if err != nil {
		t.Fatalf("read realm: %v", err)
	}
	var r realm
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode realm: %v", err)
	}
	return r
}

// TestRealmDeclaresTheConsoleClient pins what makes the console safe to
// run in a browser: a public client that can only use the authorization
// code flow with S256 PKCE, from listed origins, issuing tokens for this
// API; and an operator role the API checks.
func TestRealmDeclaresTheConsoleClient(t *testing.T) {
	t.Parallel()
	r := loadRealm(t)

	if !slices.ContainsFunc(r.Roles.Realm, func(role struct {
		Name string `json:"name"`
	}) bool {
		return role.Name == "operator"
	}) {
		t.Fatal("realm has no operator role")
	}

	i := slices.IndexFunc(r.Clients, func(c realmClient) bool { return c.ClientID == "wallet-console" })
	if i < 0 {
		t.Fatal("realm has no wallet-console client")
	}
	console := r.Clients[i]
	if !console.PublicClient || !console.StandardFlowEnabled {
		t.Fatal("wallet-console must be a public client using the authorization code flow")
	}
	if console.DirectAccessGrantsEnabled || console.ImplicitFlowEnabled || console.ServiceAccountsEnabled {
		t.Fatal("wallet-console must not allow password, implicit or client-credentials grants")
	}
	if got := console.Attributes["pkce.code.challenge.method"]; got != "S256" {
		t.Fatalf("wallet-console PKCE method = %q, want S256", got)
	}
	if len(console.RedirectURIs) == 0 || slices.ContainsFunc(console.RedirectURIs, func(uri string) bool {
		return uri == "*" || uri == "/*"
	}) {
		t.Fatalf("wallet-console redirect URIs %v must be listed origins", console.RedirectURIs)
	}
	if len(console.WebOrigins) == 0 || slices.Contains(console.WebOrigins, "*") {
		t.Fatalf("wallet-console web origins %v must be listed origins", console.WebOrigins)
	}
	if !slices.ContainsFunc(console.ProtocolMappers, func(m realmMapper) bool {
		return m.ProtocolMapper == "oidc-audience-mapper" && m.Config["included.custom.audience"] == "wallet-api"
	}) {
		t.Fatal("wallet-console tokens must carry the wallet-api audience")
	}

	if !slices.ContainsFunc(r.Users, func(u realmUser) bool {
		return u.Username == "operator" && slices.Contains(u.RealmRoles, "operator")
	}) {
		t.Fatal("realm has no demo operator user")
	}
}
