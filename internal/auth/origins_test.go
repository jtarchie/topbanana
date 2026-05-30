package auth

import (
	"reflect"
	"testing"
)

func TestBuildRPOrigins(t *testing.T) {
	cases := []struct {
		name     string
		domain   string
		port     string
		insecure bool
		want     []string
	}{
		{
			name:     "production: only https origin",
			domain:   "example.com",
			port:     "",
			insecure: false,
			want:     []string{"https://example.com"},
		},
		{
			name:     "production ignores port even when set",
			domain:   "example.com",
			port:     "8080",
			insecure: false,
			want:     []string{"https://example.com"},
		},
		{
			name:     "local dev: adds http forms with port",
			domain:   "localhost",
			port:     "8080",
			insecure: true,
			want:     []string{"https://localhost", "http://localhost", "http://localhost:8080"},
		},
		{
			name:     "local dev on default http port omits port suffix",
			domain:   "localhost",
			port:     "80",
			insecure: true,
			want:     []string{"https://localhost", "http://localhost"},
		},
		{
			name:     "local dev on default https port omits port suffix",
			domain:   "localhost",
			port:     "443",
			insecure: true,
			want:     []string{"https://localhost", "http://localhost"},
		},
		{
			name:     "local dev with empty port omits port suffix",
			domain:   "localhost",
			port:     "",
			insecure: true,
			want:     []string{"https://localhost", "http://localhost"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRPOrigins(tc.domain, tc.port, tc.insecure)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("buildRPOrigins(%q, %q, %v) = %v, want %v", tc.domain, tc.port, tc.insecure, got, tc.want)
			}
		})
	}
}
