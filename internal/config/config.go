// Package config reads the server's configuration from its environment:
//
//   - EDITY_ADDR, the address to listen on, ":8080" by default
//   - EDITY_SITES_FILE, the path of the Sites file
//   - EDITY_PUBLISHER_GITHUB_APP_ID and EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY,
//     the edity-publisher GitHub App's ID and PEM private key
package config

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/RMYPG/edity-server/internal/site"
)

// Config is what the server needs to start.
type Config struct {
	// Addr is the address the server listens on.
	Addr  string
	Sites *site.Registry
	// GitHubApp is the edity-publisher App, which commits each Publish.
	GitHubApp GitHubApp
}

// GitHubApp holds the credentials of a GitHub App.
type GitHubApp struct {
	ID         int64
	PrivateKey *rsa.PrivateKey
}

// Load reads the configuration from the environment variables that getenv
// returns, and reports every missing or invalid one at once.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	lookup := func(name string) string {
		value := getenv(name)
		if value == "" {
			errs = append(errs, fmt.Errorf("%s is not set", name))
		}
		return value
	}

	cfg := Config{Addr: getenv("EDITY_ADDR")}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if path := lookup("EDITY_SITES_FILE"); path != "" {
		sites, err := readSites(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("EDITY_SITES_FILE: %w", err))
		}
		cfg.Sites = sites
	}
	if id := lookup("EDITY_PUBLISHER_GITHUB_APP_ID"); id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("EDITY_PUBLISHER_GITHUB_APP_ID is not a GitHub App ID: %q", id))
		}
		cfg.GitHubApp.ID = n
	}
	if key := lookup("EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY"); key != "" {
		k, err := parsePrivateKey(key)
		if err != nil {
			errs = append(errs, fmt.Errorf("EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY %w", err))
		}
		cfg.GitHubApp.PrivateKey = k
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func readSites(path string) (*site.Registry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The Developer chooses the path.
	if err != nil {
		return nil, err
	}
	return site.Parse(bytes.NewReader(data))
}

// parsePrivateKey reads a PEM RSA key in the PKCS #1 form GitHub hands out,
// or in PKCS #8. Its errors never quote the key.
func parsePrivateKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("is not a PEM private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := key.(*rsa.PrivateKey); ok {
			return key, nil
		}
	}
	return nil, errors.New("is not an RSA private key")
}
