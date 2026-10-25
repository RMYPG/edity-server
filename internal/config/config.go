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
	// Publisher is the edity-publisher GitHub App, which commits each Publish.
	Publisher GitHubApp
}

// GitHubApp holds the credentials of a GitHub App.
type GitHubApp struct {
	ID         int64
	PrivateKey *rsa.PrivateKey
}

// Load reads the configuration from the environment variables that getenv
// returns, and reports every missing or invalid one at once.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{Addr: getenv("EDITY_ADDR")}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}

	var errs []error
	read := func(name string, parse func(string) error) {
		value := getenv(name)
		if value == "" {
			errs = append(errs, fmt.Errorf("%s: not set", name))
		} else if err := parse(value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	read("EDITY_SITES_FILE", func(path string) (err error) {
		cfg.Sites, err = readSites(path)
		return err
	})
	read("EDITY_PUBLISHER_GITHUB_APP_ID", func(id string) (err error) {
		cfg.Publisher.ID, err = parseAppID(id)
		return err
	})
	read("EDITY_PUBLISHER_GITHUB_APP_PRIVATE_KEY", func(key string) (err error) {
		cfg.Publisher.PrivateKey, err = parsePrivateKey(key)
		return err
	})

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

func parseAppID(text string) (int64, error) {
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a GitHub App ID", text)
	}
	return id, nil
}

// parsePrivateKey reads a PEM RSA key in the PKCS #1 form GitHub hands out,
// or in PKCS #8. Its errors never quote the key.
func parsePrivateKey(text string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("not a PEM private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if key, ok := key.(*rsa.PrivateKey); ok {
			return key, nil
		}
	}
	return nil, errors.New("not an RSA private key")
}
