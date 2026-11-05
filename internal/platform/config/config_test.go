package config

import (
	"strings"
	"testing"
)

const testDBURL = "postgres://u:p@db:5432/dining"

// withDB returns env with a valid DATABASE_URL added, so cases that test
// other variables don't have to repeat it.
func withDB(env map[string]string) map[string]string {
	out := map[string]string{"DATABASE_URL": testDBURL}
	for k, v := range env {
		out[k] = v
	}
	return out
}

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr []string // substrings expected in the error; nil means no error
	}{
		{
			name: "defaults when only DATABASE_URL is set",
			env:  withDB(nil),
			want: Config{AppEnv: EnvLocal, HTTPPort: 8080, DatabaseURL: testDBURL},
		},
		{
			name: "empty values fall back to defaults",
			env:  withDB(map[string]string{"APP_ENV": "", "HTTP_PORT": ""}),
			want: Config{AppEnv: EnvLocal, HTTPPort: 8080, DatabaseURL: testDBURL},
		},
		{
			name: "explicit valid values",
			env:  withDB(map[string]string{"APP_ENV": EnvProduction, "HTTP_PORT": "9090"}),
			want: Config{AppEnv: EnvProduction, HTTPPort: 9090, DatabaseURL: testDBURL},
		},
		{
			name: "port lower bound",
			env:  withDB(map[string]string{"HTTP_PORT": "1"}),
			want: Config{AppEnv: EnvLocal, HTTPPort: 1, DatabaseURL: testDBURL},
		},
		{
			name: "port upper bound",
			env:  withDB(map[string]string{"HTTP_PORT": "65535"}),
			want: Config{AppEnv: EnvLocal, HTTPPort: 65535, DatabaseURL: testDBURL},
		},
		{
			name:    "missing database url",
			env:     map[string]string{},
			wantErr: []string{"DATABASE_URL", "required"},
		},
		{
			name:    "empty database url",
			env:     map[string]string{"DATABASE_URL": ""},
			wantErr: []string{"DATABASE_URL", "required"},
		},
		{
			name:    "unsupported app env",
			env:     withDB(map[string]string{"APP_ENV": "prod"}),
			wantErr: []string{"APP_ENV", `"prod"`},
		},
		{
			name:    "app env is case sensitive",
			env:     withDB(map[string]string{"APP_ENV": "Production"}),
			wantErr: []string{"APP_ENV"},
		},
		{
			name:    "non numeric port",
			env:     withDB(map[string]string{"HTTP_PORT": "abc"}),
			wantErr: []string{"HTTP_PORT", "not a valid integer"},
		},
		{
			name:    "port zero",
			env:     withDB(map[string]string{"HTTP_PORT": "0"}),
			wantErr: []string{"HTTP_PORT", "out of range"},
		},
		{
			name:    "port too large",
			env:     withDB(map[string]string{"HTTP_PORT": "65536"}),
			wantErr: []string{"HTTP_PORT", "out of range"},
		},
		{
			name:    "negative port",
			env:     withDB(map[string]string{"HTTP_PORT": "-1"}),
			wantErr: []string{"HTTP_PORT", "out of range"},
		},
		{
			name:    "all errors are reported together",
			env:     map[string]string{"APP_ENV": "dev", "HTTP_PORT": "x"},
			wantErr: []string{"APP_ENV", "HTTP_PORT", "DATABASE_URL"},
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
				if got != (Config{}) {
					t.Errorf("expected zero Config on error, got %+v", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
