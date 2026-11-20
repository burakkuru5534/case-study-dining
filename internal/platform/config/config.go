// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	EnvLocal      = "local"
	EnvTest       = "test"
	EnvStaging    = "staging"
	EnvProduction = "production"

	defaultAppEnv   = EnvLocal
	defaultHTTPPort = 8080

	// MinWebhookSecretLength is the minimum HMAC secret length in bytes
	// (256 bits, matching the SHA-256 output size).
	MinWebhookSecretLength = 32
)

// Config holds the runtime configuration of the API process.
type Config struct {
	AppEnv      string
	HTTPPort    int
	DatabaseURL string

	// WebhookSecretsReference are the accepted HMAC secrets for the
	// reference provider. More than one allows rotation without downtime:
	// add the new secret, switch the sender, then remove the old one.
	WebhookSecretsReference []string
}

// Load reads the API configuration from the process environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

// LoadDatabaseURL reads only DATABASE_URL, for commands such as migrate that
// need nothing else (and must not require API secrets).
func LoadDatabaseURL() (string, error) {
	v := os.Getenv("DATABASE_URL")
	if v == "" {
		return "", errors.New("invalid configuration: DATABASE_URL: required")
	}
	return v, nil
}

// load is separated from Load so tests can inject an environment
// without mutating process-wide state (which would prevent t.Parallel).
// An empty value is treated the same as an unset variable.
func load(getenv func(string) string) (Config, error) {
	cfg := Config{
		AppEnv:      defaultAppEnv,
		HTTPPort:    defaultHTTPPort,
		DatabaseURL: getenv("DATABASE_URL"),
	}

	var errs []error

	if v := getenv("APP_ENV"); v != "" {
		switch v {
		case EnvLocal, EnvTest, EnvStaging, EnvProduction:
			cfg.AppEnv = v
		default:
			errs = append(errs, fmt.Errorf("APP_ENV: unsupported value %q (allowed: %s, %s, %s, %s)",
				v, EnvLocal, EnvTest, EnvStaging, EnvProduction))
		}
	}

	if v := getenv("HTTP_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("HTTP_PORT: %q is not a valid integer", v))
		case port < 1 || port > 65535:
			errs = append(errs, fmt.Errorf("HTTP_PORT: %d is out of range 1-65535", port))
		default:
			cfg.HTTPPort = port
		}
	}

	// Only presence is checked here; the URL format is validated by the
	// database driver when the pool is created.
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL: required"))
	}

	secrets, err := parseSecrets("WEBHOOK_SECRET_REFERENCE", getenv("WEBHOOK_SECRET_REFERENCE"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.WebhookSecretsReference = secrets

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// parseSecrets splits a comma-separated list of secrets. Error messages
// never include secret values.
func parseSecrets(name, raw string) ([]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("%s: required", name)
	}
	parts := strings.Split(raw, ",")
	secrets := make([]string, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
			return nil, fmt.Errorf("%s: entry %d is empty", name, i+1)
		case len(p) < MinWebhookSecretLength:
			return nil, fmt.Errorf("%s: entry %d is shorter than %d bytes", name, i+1, MinWebhookSecretLength)
		}
		secrets = append(secrets, p)
	}
	return secrets, nil
}
