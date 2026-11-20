// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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

	// APIReadTokens are the accepted bearer tokens for the read API
	// (/v1/...). Several values allow rotation without downtime.
	APIReadTokens []string
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

	tokens, err := parseSecrets("API_READ_TOKENS", getenv("API_READ_TOKENS"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.APIReadTokens = tokens

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

// WorkerConfig holds the configuration of the event worker process.
type WorkerConfig struct {
	DatabaseURL  string
	Concurrency  int
	PollInterval time.Duration
	MaxAttempts  int
}

// LoadWorker reads the worker configuration. The worker needs no webhook secrets.
func LoadWorker() (WorkerConfig, error) {
	return loadWorker(os.Getenv)
}

func loadWorker(getenv func(string) string) (WorkerConfig, error) {
	cfg := WorkerConfig{
		DatabaseURL:  getenv("DATABASE_URL"),
		Concurrency:  2,
		PollInterval: time.Second,
		MaxAttempts:  10,
	}
	var errs []error

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL: required"))
	}
	if v := getenv("WORKER_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("WORKER_CONCURRENCY: %q is not a valid integer", v))
		case n < 1 || n > 64:
			errs = append(errs, fmt.Errorf("WORKER_CONCURRENCY: %d is out of range 1-64", n))
		default:
			cfg.Concurrency = n
		}
	}
	if v := getenv("WORKER_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("WORKER_POLL_INTERVAL: %q is not a duration (e.g. 500ms, 2s)", v))
		case d < 10*time.Millisecond || d > time.Minute:
			errs = append(errs, fmt.Errorf("WORKER_POLL_INTERVAL: %s is out of range 10ms-1m", d))
		default:
			cfg.PollInterval = d
		}
	}
	if v := getenv("WORKER_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("WORKER_MAX_ATTEMPTS: %q is not a valid integer", v))
		case n < 1 || n > 100:
			errs = append(errs, fmt.Errorf("WORKER_MAX_ATTEMPTS: %d is out of range 1-100", n))
		default:
			cfg.MaxAttempts = n
		}
	}

	if err := errors.Join(errs...); err != nil {
		return WorkerConfig{}, fmt.Errorf("invalid worker configuration: %w", err)
	}
	return cfg, nil
}
