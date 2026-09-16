package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestNewBindsOriginsAndValidatesConfig(t *testing.T) {
	token := map[string]any{"token": "secret"}
	for remote, want := range map[string][3]string{
		"https://github.com/example/catalog.git":     {"https://api.github.com", "example", "catalog"},
		"git@github.com:example/catalog.git":         {"https://api.github.com", "example", "catalog"},
		"ssh://git@ghe.example.com/example/catalog":  {"https://ghe.example.com/api/v3", "example", "catalog"},
		"http://127.0.0.1:8080/example/catalog.git/": {"http://127.0.0.1:8080/api/v3", "example", "catalog"},
	} {
		client, err := New(remote, token)
		if err != nil {
			t.Fatalf("%s: %v", remote, err)
		}
		if got := [3]string{client.api, client.owner, client.repository}; got != want {
			t.Fatalf("%s: %v", remote, got)
		}
	}
	for _, remote := range []string{"https://github.com/example", "example/catalog", "/srv/git/catalog.git", "https://github.com/example/group/catalog.git"} {
		if _, err := New(remote, token); err == nil {
			t.Fatalf("accepted origin %q", remote)
		}
	}
	for name, settings := range map[string]map[string]any{
		"none":              {},
		"both":              {"token": "secret", "client_id": "Iv1.x", "installation_id": 7, "private_key": "key"},
		"incomplete-app":    {"client_id": "Iv1.x", "private_key": "key"},
		"legacy-app-id":     {"app_id": 1, "installation_id": 7, "private_key": "key"},
		"bad-installation":  {"client_id": "Iv1.x", "installation_id": "seven", "private_key": "key"},
		"zero-installation": {"client_id": "Iv1.x", "installation_id": 0, "private_key": fixtureKey(t)},
		"unparseable-key":   {"client_id": "Iv1.x", "installation_id": 7, "private_key": "not a key"},
	} {
		if _, err := New("https://github.com/example/catalog.git", settings); err == nil || !strings.HasPrefix(err.Error(), "github: ") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestIdentityFollowsTheCredentials checks an App mints one token for its
// configured installation, reuses it and signs as its bot user, while a token
// signs as its user.
func TestIdentityFollowsTheCredentials(t *testing.T) {
	var minted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var claims jwt.RegisteredClaims
		_, _, jwtErr := jwt.NewParser().ParseUnverified(authorization, &claims)
		switch r.URL.Path {
		case "/api/v3/app":
			if jwtErr != nil || claims.Issuer != "Iv1.fixture" {
				http.Error(w, "needs an App assertion", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"slug": "stemma-fixture"})
		case "/api/v3/app/installations/7/access_tokens":
			if jwtErr != nil || r.Method != http.MethodPost {
				http.Error(w, "needs an App assertion", http.StatusUnauthorized)
				return
			}
			minted.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted", "expires_at": time.Now().Add(time.Hour)})
		case "/api/v3/users/stemma-fixture[bot]":
			if authorization != "minted" {
				http.Error(w, "needs the installation token", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 424242, "login": "stemma-fixture[bot]"})
		case "/api/v3/user":
			if authorization != "secret" {
				http.Error(w, "needs the token", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 583231, "login": "octocat"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	remote := server.URL + "/example/catalog.git"

	app, err := New(remote, map[string]any{"client_id": "Iv1.fixture", "installation_id": 7, "private_key": fixtureKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		identity, err := app.Identity(t.Context())
		if err != nil || identity.Name != "stemma-fixture[bot]" || identity.Email != "424242+stemma-fixture[bot]@users.noreply.github.com" {
			t.Fatalf("app identity %+v: %v", identity, err)
		}
		if user, password, err := app.Credentials(t.Context()); err != nil || user != "x-access-token" || password != "minted" {
			t.Fatalf("app credentials %s %s: %v", user, password, err)
		}
	}
	if minted.Load() != 1 {
		t.Fatalf("minted %d installation tokens", minted.Load())
	}

	user, err := New(remote, map[string]any{"token": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := user.Identity(t.Context())
	if err != nil || identity.Name != "octocat" || identity.Email != "583231+octocat@users.noreply.github.com" {
		t.Fatalf("user identity %+v: %v", identity, err)
	}
}

// fixtureKey is a PEM key in the PKCS#1 form GitHub issues.
func fixtureKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
