package config_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RMYPG/edity-server/internal/config"
)

const sandbox = `{
  "sites": [
    {
      "id": "edity-sandbox",
      "hostnames": ["sandbox.example.com"],
      "githubRepository": "RMYPG/edity-sandbox",
      "locale": "en",
      "zitadelRole": "site:edity-sandbox",
      "kubernetes": {"namespace": "edity-sandbox", "deployment": "edity-sandbox-sws"}
    }
  ]
}`

func TestLoadReadsTheSitesAndThePublisherApp(t *testing.T) {
	key := newKey(t)
	env := validEnv(t)
	env["EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY"] = pkcs1(key)

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if s, ok := cfg.Sites.Lookup("sandbox.example.com"); !ok || s.ID != "edity-sandbox" {
		t.Errorf("Sites.Lookup(sandbox.example.com) = %q, %v, want edity-sandbox", s.ID, ok)
	}
	if cfg.Publisher.ID != 123456 {
		t.Errorf("Publisher.ID = %d, want 123456", cfg.Publisher.ID)
	}
	if !key.Equal(cfg.Publisher.PrivateKey) {
		t.Error("Publisher.PrivateKey is not the key in EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY")
	}
}

func TestLoadReadsZitadelAndTheSessionKey(t *testing.T) {
	cfg, err := config.Load(getenv(validEnv(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Zitadel.Issuer != "https://zitadel.example.com" {
		t.Errorf("Zitadel.Issuer = %q, want https://zitadel.example.com", cfg.Zitadel.Issuer)
	}
	if cfg.Zitadel.ClientID != "337@edity" {
		t.Errorf("Zitadel.ClientID = %q, want 337@edity", cfg.Zitadel.ClientID)
	}
	if want := []byte("0123456789abcdef0123456789abcdef"); !bytes.Equal(cfg.SessionKey, want) {
		t.Errorf("SessionKey = %q, want %q", cfg.SessionKey, want)
	}
}

func TestLoadReadsTheProjectAndTheServiceUsersKey(t *testing.T) {
	key := newKey(t)
	env := validEnv(t)
	env["EDITY_ZITADEL_SERVICE_KEY"] = serviceKey(key)

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Zitadel.ProjectID != "340000000000000001" {
		t.Errorf("Zitadel.ProjectID = %q, want 340000000000000001", cfg.Zitadel.ProjectID)
	}
	user := cfg.Zitadel.ServiceUser
	if user.ID != "350000000000000002" || user.KeyID != "360000000000000003" {
		t.Errorf("Zitadel.ServiceUser = %q with the key %q, want 350000000000000002 with the key 360000000000000003", user.ID, user.KeyID)
	}
	if !key.Equal(user.PrivateKey) {
		t.Error("Zitadel.ServiceUser.PrivateKey is not the key in EDITY_ZITADEL_SERVICE_KEY")
	}
}

func TestLoadAcceptsAnIssuerOverHTTPOnlyOnLoopback(t *testing.T) {
	env := validEnv(t)
	env["EDITY_ZITADEL_ISSUER"] = "http://localhost:8081"

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Zitadel.Issuer != "http://localhost:8081" {
		t.Errorf("Zitadel.Issuer = %q, want http://localhost:8081", cfg.Zitadel.Issuer)
	}
}

func TestLoadListensOnEdityAddr(t *testing.T) {
	env := validEnv(t)
	env["EDITY_ADDR"] = "127.0.0.1:9000"

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("Addr = %q, want 127.0.0.1:9000", cfg.Addr)
	}
}

func TestLoadAcceptsAPKCS8PrivateKey(t *testing.T) {
	key := newKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	env := validEnv(t)
	env["EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	cfg, err := config.Load(getenv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !key.Equal(cfg.Publisher.PrivateKey) {
		t.Error("Publisher.PrivateKey is not the key in EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY")
	}
}

func TestLoadNamesEveryMissingVariable(t *testing.T) {
	_, err := config.Load(getenv(nil))
	if err == nil {
		t.Fatal("Load accepted an empty environment")
	}
	for _, name := range []string{
		"EDITY_SITES_FILE", "EDITY_PUBLISHER_GITHUB_APP_ID", "EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY",
		"EDITY_ZITADEL_ISSUER", "EDITY_ZITADEL_CLIENT_ID", "EDITY_ZITADEL_PROJECT_ID", "EDITY_ZITADEL_SERVICE_KEY", "EDITY_SESSION_KEY",
	} {
		if !strings.Contains(err.Error(), name+": not set") {
			t.Errorf("Load error = %q, want it to say %s is not set", err, name)
		}
	}
}

func TestLoadRejectsABadVariable(t *testing.T) {
	notAKey := "-----BEGIN RSA PRIVATE KEY-----\nc2VjcmV0LWtleS1ieXRlcw==\n-----END RSA PRIVATE KEY-----\n"
	notAJSONKey := "not a Zitadel service user's JSON key"
	tests := []struct {
		name, variable, value, want string
	}{
		{"missing Sites file", "EDITY_SITES_FILE", filepath.Join(t.TempDir(), "missing.json"), "EDITY_SITES_FILE: open "},
		{"invalid Sites file", "EDITY_SITES_FILE", writeFile(t, `{"sites": []}`), "EDITY_SITES_FILE: the Sites file lists no Sites"},
		{"App ID not a number", "EDITY_PUBLISHER_GITHUB_APP_ID", "edity-publisher", "EDITY_PUBLISHER_GITHUB_APP_ID: not a GitHub App ID"},
		{"App ID zero", "EDITY_PUBLISHER_GITHUB_APP_ID", "0", "EDITY_PUBLISHER_GITHUB_APP_ID: not a GitHub App ID"},
		{"App ID swapped with the key", "EDITY_PUBLISHER_GITHUB_APP_ID", notAKey, "EDITY_PUBLISHER_GITHUB_APP_ID: not a GitHub App ID"},
		{"key not PEM", "EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY", "c2VjcmV0LWtleS1ieXRlcw==", "EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY: not a PEM private key"},
		{"key not RSA", "EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY", notAKey, "EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY: not an RSA private key"},
		{"issuer not a URL", "EDITY_ZITADEL_ISSUER", "zitadel.example.com", "EDITY_ZITADEL_ISSUER: not an https URL"},
		{"issuer over http", "EDITY_ZITADEL_ISSUER", "http://zitadel.example.com", "EDITY_ZITADEL_ISSUER: not an https URL"},
		{"issuer with a query", "EDITY_ZITADEL_ISSUER", "https://zitadel.example.com?a=b", "EDITY_ZITADEL_ISSUER: not an https URL"},
		{"issuer with a trailing slash", "EDITY_ZITADEL_ISSUER", "https://zitadel.example.com/", "EDITY_ZITADEL_ISSUER: ends with a slash"},
		{"session key not base64", "EDITY_SESSION_KEY", "c2VjcmV0LWtleS1ieXRlcw==!", "EDITY_SESSION_KEY: not 32 bytes in base64"},
		{"session key too short", "EDITY_SESSION_KEY", "c2VjcmV0LWtleS1ieXRlcw==", "EDITY_SESSION_KEY: not 32 bytes in base64"},
		{"service key not JSON", "EDITY_ZITADEL_SERVICE_KEY", "c2VjcmV0LWtleS1ieXRlcw==", "EDITY_ZITADEL_SERVICE_KEY: " + notAJSONKey},
		{"service key with no user", "EDITY_ZITADEL_SERVICE_KEY", jsonKey(map[string]string{"keyId": "1", "key": notAKey}), "EDITY_ZITADEL_SERVICE_KEY: " + notAJSONKey},
		{"service key with no ID", "EDITY_ZITADEL_SERVICE_KEY", jsonKey(map[string]string{"userId": "1", "key": notAKey}), "EDITY_ZITADEL_SERVICE_KEY: " + notAJSONKey},
		{"service key with no key", "EDITY_ZITADEL_SERVICE_KEY", jsonKey(map[string]string{"userId": "1", "keyId": "1"}), "EDITY_ZITADEL_SERVICE_KEY: " + notAJSONKey},
		{"an application's key", "EDITY_ZITADEL_SERVICE_KEY", jsonKey(map[string]string{"type": "application", "keyId": "1", "appId": "2", "clientId": "3", "key": notAKey}), "EDITY_ZITADEL_SERVICE_KEY: " + notAJSONKey},
		{"service key not PEM", "EDITY_ZITADEL_SERVICE_KEY", jsonKey(map[string]string{"userId": "1", "keyId": "1", "key": "c2VjcmV0LWtleS1ieXRlcw=="}), "EDITY_ZITADEL_SERVICE_KEY: not a PEM private key"},
		{"service key not RSA", "EDITY_ZITADEL_SERVICE_KEY", jsonKey(map[string]string{"userId": "1", "keyId": "1", "key": notAKey}), "EDITY_ZITADEL_SERVICE_KEY: not an RSA private key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv(t)
			env[tt.variable] = tt.value

			_, err := config.Load(getenv(env))
			if err == nil {
				t.Fatalf("Load accepted %s=%q", tt.variable, tt.value)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load error = %q, want it to mention %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "c2VjcmV0") {
				t.Errorf("Load error = %q leaks the private key", err)
			}
		})
	}
}

func validEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"EDITY_SITES_FILE":                       writeFile(t, sandbox),
		"EDITY_PUBLISHER_GITHUB_APP_ID":          "123456",
		"EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY": pkcs1(newKey(t)),
		"EDITY_ZITADEL_ISSUER":                   "https://zitadel.example.com",
		"EDITY_ZITADEL_CLIENT_ID":                "337@edity",
		"EDITY_ZITADEL_PROJECT_ID":               "340000000000000001",
		"EDITY_ZITADEL_SERVICE_KEY":              serviceKey(newKey(t)),
		"EDITY_SESSION_KEY":                      base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	}
}

func getenv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sites.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// serviceKey is key in the JSON file Zitadel hands out for a service user.
func serviceKey(key *rsa.PrivateKey) string {
	return jsonKey(map[string]string{
		"type": "serviceaccount", "keyId": "360000000000000003", "userId": "350000000000000002", "key": pkcs1(key),
	})
}

func jsonKey(fields map[string]string) string {
	b, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// pkcs1 encodes key the way GitHub hands out App private keys.
func pkcs1(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
