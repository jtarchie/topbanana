package build

import "testing"

func TestAgentRunner_SiteURL(t *testing.T) {
	cases := []struct {
		name     string
		domain   string
		port     string
		insecure bool
		slug     string
		want     string
	}{
		{
			name:     "local dev with non-default port",
			domain:   "localhost",
			port:     "8080",
			insecure: true,
			slug:     "myapp",
			want:     "http://myapp.localhost:8080",
		},
		{
			name:     "local dev on default http port omits suffix",
			domain:   "localhost",
			port:     "80",
			insecure: true,
			slug:     "myapp",
			want:     "http://myapp.localhost",
		},
		{
			name:     "local dev on default https port omits suffix",
			domain:   "localhost",
			port:     "443",
			insecure: true,
			slug:     "myapp",
			want:     "http://myapp.localhost",
		},
		{
			name:     "production: https, no port",
			domain:   "bloomhollow.io",
			port:     "443",
			insecure: false,
			slug:     "myapp",
			want:     "https://myapp.bloomhollow.io",
		},
		{
			name:     "production ignores port even when set",
			domain:   "bloomhollow.io",
			port:     "8443",
			insecure: false,
			slug:     "myapp",
			want:     "https://myapp.bloomhollow.io",
		},
		{
			name:     "empty domain yields empty URL (legacy test ctor)",
			domain:   "",
			port:     "8080",
			insecure: true,
			slug:     "myapp",
			want:     "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := agentRunner{domain: tc.domain, port: tc.port, insecure: tc.insecure}
			if got := r.siteURL(tc.slug); got != tc.want {
				t.Errorf("siteURL(%q) = %q, want %q", tc.slug, got, tc.want)
			}
		})
	}
}
