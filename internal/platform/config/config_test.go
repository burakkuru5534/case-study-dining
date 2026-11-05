package config

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr []string // substrings expected in the error; nil means no error
	}{
		{
			name: "defaults when nothing is set",
			env:  map[string]string{},
			want: Config{AppEnv: EnvLocal, HTTPPort: 8080},
		},
		{
			name: "empty values fall back to defaults",
			env:  map[string]string{"APP_ENV": "", "HTTP_PORT": ""},
			want: Config{AppEnv: EnvLocal, HTTPPort: 8080},
		},
		{
			name: "explicit valid values",
			env: map[string]string{
				"APP_ENV":      EnvProduction,
				"HTTP_PORT":    "9090",
				"DATABASE_URL": "postgres://u:p@db:5432/dining",
			},
			want: Config{AppEnv: EnvProduction, HTTPPort: 9090, DatabaseURL: "postgres://u:p@db:5432/dining"},
		},
		{
			name: "port lower bound",
			env:  map[string]string{"HTTP_PORT": "1"},
			want: Config{AppEnv: EnvLocal, HTTPPort: 1},
		},
		{
			name: "port upper bound",
			env:  map[string]string{"HTTP_PORT": "65535"},
			want: Config{AppEnv: EnvLocal, HTTPPort: 65535},
		},
		{
			name:    "unsupported app env",
			env:     map[string]string{"APP_ENV": "prod"},
			wantErr: []string{"APP_ENV", `"prod"`},
		},
		{
			name:    "app env is case sensitive",
			env:     map[string]string{"APP_ENV": "Production"},
			wantErr: []string{"APP_ENV"},
		},
		{
			name:    "non numeric port",
			env:     map[string]string{"HTTP_PORT": "abc"},
			wantErr: []string{"HTTP_PORT", "not a valid integer"},
		},
		{
			name:    "port zero",
			env:     map[string]string{"HTTP_PORT": "0"},
			wantErr: []string{"HTTP_PORT", "out of range"},
		},
		{
			name:    "port too large",
			env:     map[string]string{"HTTP_PORT": "65536"},
			wantErr: []string{"HTTP_PORT", "out of range"},
		},
		{
			name:    "negative port",
			env:     map[string]string{"HTTP_PORT": "-1"},
			wantErr: []string{"HTTP_PORT", "out of range"},
		},
		{
			name:    "all errors are reported together",
			env:     map[string]string{"APP_ENV": "dev", "HTTP_PORT": "x"},
			wantErr: []string{"APP_ENV", "HTTP_PORT"},
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
