// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

const (
	EnvLocal      = "local"
	EnvTest       = "test"
	EnvStaging    = "staging"
	EnvProduction = "production"

	defaultAppEnv   = EnvLocal
	defaultHTTPPort = 8080
)

// Config holds the runtime configuration of the API process.
type Config struct {
	AppEnv      string
	HTTPPort    int
	DatabaseURL string
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return load(os.Getenv)
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

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}
