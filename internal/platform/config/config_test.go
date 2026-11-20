package config

import (
	"reflect"
	"strings"
	"testing"
)

const (
	testDBURL   = "postgres://u:p@db:5432/dining"
	testSecret  = "0123456789abcdef0123456789abcdef" // exactly 32 bytes
	testSecret2 = "fedcba9876543210fedcba9876543210"
)

// withRequired returns env with valid DATABASE_URL and webhook secret added,
// so cases that test other variables don't have to repeat them.
func withRequired(env map[string]string) map[string]string {
	out := map[string]string{"DATABASE_URL": testDBURL, "WEBHOOK_SECRET_REFERENCE": testSecret}
	for k, v := range env {
		out[k] = v
	}
	return out
}

func cfg(appEnv string, port int, secrets ...string) Config {
	if len(secrets) == 0 {
		secrets = []string{testSecret}
	}
	return Config{AppEnv: appEnv, HTTPPort: port, DatabaseURL: testDBURL, WebhookSecretsReference: secrets}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr []string // substrings expected in the error; nil means no error
	}{
		{name: "defaults when only required values are set", env: withRequired(nil), want: cfg(EnvLocal, 8080)},
		{name: "empty values fall back to defaults", env: withRequired(map[string]string{"APP_ENV": "", "HTTP_PORT": ""}), want: cfg(EnvLocal, 8080)},
		{name: "explicit valid values", env: withRequired(map[string]string{"APP_ENV": EnvProduction, "HTTP_PORT": "9090"}), want: cfg(EnvProduction, 9090)},
		{name: "port lower bound", env: withRequired(map[string]string{"HTTP_PORT": "1"}), want: cfg(EnvLocal, 1)},
		{name: "port upper bound", env: withRequired(map[string]string{"HTTP_PORT": "65535"}), want: cfg(EnvLocal, 65535)},
		{
			name: "two secrets for rotation, whitespace trimmed",
			env:  withRequired(map[string]string{"WEBHOOK_SECRET_REFERENCE": " " + testSecret + " , " + testSecret2}),
			want: cfg(EnvLocal, 8080, testSecret, testSecret2),
		},

		{name: "missing database url", env: map[string]string{"WEBHOOK_SECRET_REFERENCE": testSecret}, wantErr: []string{"DATABASE_URL", "required"}},
		{name: "empty database url", env: withRequired(map[string]string{"DATABASE_URL": ""}), wantErr: []string{"DATABASE_URL", "required"}},
		{name: "missing webhook secret", env: map[string]string{"DATABASE_URL": testDBURL}, wantErr: []string{"WEBHOOK_SECRET_REFERENCE: required"}},
		{name: "short webhook secret", env: withRequired(map[string]string{"WEBHOOK_SECRET_REFERENCE": "too-short"}), wantErr: []string{"WEBHOOK_SECRET_REFERENCE: entry 1 is shorter than 32 bytes"}},
		{name: "empty entry in secret list", env: withRequired(map[string]string{"WEBHOOK_SECRET_REFERENCE": testSecret + ","}), wantErr: []string{"WEBHOOK_SECRET_REFERENCE: entry 2 is empty"}},
		{name: "short second secret", env: withRequired(map[string]string{"WEBHOOK_SECRET_REFERENCE": testSecret + ",short"}), wantErr: []string{"entry 2 is shorter"}},
		{name: "unsupported app env", env: withRequired(map[string]string{"APP_ENV": "prod"}), wantErr: []string{"APP_ENV", `"prod"`}},
		{name: "app env is case sensitive", env: withRequired(map[string]string{"APP_ENV": "Production"}), wantErr: []string{"APP_ENV"}},
		{name: "non numeric port", env: withRequired(map[string]string{"HTTP_PORT": "abc"}), wantErr: []string{"HTTP_PORT", "not a valid integer"}},
		{name: "port zero", env: withRequired(map[string]string{"HTTP_PORT": "0"}), wantErr: []string{"HTTP_PORT", "out of range"}},
		{name: "port too large", env: withRequired(map[string]string{"HTTP_PORT": "65536"}), wantErr: []string{"HTTP_PORT", "out of range"}},
		{name: "negative port", env: withRequired(map[string]string{"HTTP_PORT": "-1"}), wantErr: []string{"HTTP_PORT", "out of range"}},
		{
			name:    "all errors are reported together",
			env:     map[string]string{"APP_ENV": "dev", "HTTP_PORT": "x"},
			wantErr: []string{"APP_ENV", "HTTP_PORT", "DATABASE_URL", "WEBHOOK_SECRET_REFERENCE"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := load(func(k string) string { return tt.env[k] })

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error, got nil (config: %+v)", got)
				}
				for _, s := range tt.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error %q does not contain %q", err, s)
					}
				}
				if !reflect.DeepEqual(got, Config{}) {
					t.Errorf("expected zero Config on error, got %+v", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadErrorsDoNotLeakSecrets(t *testing.T) {
	t.Parallel()

	const leaked = "short-but-secret"
	_, err := load(func(k string) string {
		return withRequired(map[string]string{"WEBHOOK_SECRET_REFERENCE": testSecret + "," + leaked})[k]
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), leaked) || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("error leaks a secret: %v", err)
	}
}
