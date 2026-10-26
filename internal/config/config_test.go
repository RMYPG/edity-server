package config_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
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
	env := map[string]string{
		"EDITY_SITES_FILE":                       writeFile(t, sandbox),
		"EDITY_PUBLISHER_GITHUB_APP_ID":          "123456",
		"EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY": pkcs1(key),
	}

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
	for _, name := range []string{"EDITY_SITES_FILE", "EDITY_PUBLISHER_GITHUB_APP_ID", "EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY"} {
		if !strings.Contains(err.Error(), name+": not set") {
			t.Errorf("Load error = %q, want it to say %s is not set", err, name)
		}
	}
}

func TestLoadRejectsABadVariable(t *testing.T) {
	notAKey := "-----BEGIN RSA PRIVATE KEY-----\nc2VjcmV0LWtleS1ieXRlcw==\n-----END RSA PRIVATE KEY-----\n"
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

// pkcs1 encodes key the way GitHub hands out App private keys.
func pkcs1(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}
