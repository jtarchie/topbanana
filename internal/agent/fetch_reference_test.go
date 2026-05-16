package agent

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestValidateReferenceURL(t *testing.T) {
	public := func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	privateLookup := func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.0.0.5")}, nil
	}
	mixedLookup := func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("127.0.0.1")}, nil
	}
	errLookup := func(host string) ([]net.IP, error) {
		return nil, errors.New("nxdomain")
	}
	emptyLookup := func(host string) ([]net.IP, error) {
		return nil, nil
	}

	cases := []struct {
		name      string
		input     string
		resolve   func(string) ([]net.IP, error)
		wantErr   string // substring; empty means no error
		wantFinal string // expected u.String() when no error
	}{
		{
			name:      "https public host",
			input:     "https://example.com/path?q=1",
			resolve:   public,
			wantFinal: "https://example.com/path?q=1",
		},
		{
			name:      "http public host",
			input:     "http://example.com",
			resolve:   public,
			wantFinal: "http://example.com",
		},
		{
			name:    "rejects ftp scheme",
			input:   "ftp://example.com",
			resolve: public,
			wantErr: "unsupported scheme",
		},
		{
			name:    "rejects file scheme",
			input:   "file:///etc/passwd",
			resolve: public,
			wantErr: "unsupported scheme",
		},
		{
			name:    "rejects missing host",
			input:   "https://",
			resolve: public,
			wantErr: "missing host",
		},
		{
			name:    "rejects localhost literal",
			input:   "http://localhost:8080",
			resolve: public,
			wantErr: "localhost",
		},
		{
			name:    "rejects loopback ip literal",
			input:   "http://127.0.0.1",
			resolve: public,
			wantErr: "loopback",
		},
		{
			name:    "rejects ipv6 loopback literal",
			input:   "http://[::1]/",
			resolve: public,
			wantErr: "loopback",
		},
		{
			name:    "rejects private ip literal",
			input:   "http://10.0.0.1",
			resolve: public,
			wantErr: "private",
		},
		{
			name:    "rejects link-local literal",
			input:   "http://169.254.169.254/latest/meta-data/",
			resolve: public,
			wantErr: "link-local",
		},
		{
			name:    "rejects unspecified ip",
			input:   "http://0.0.0.0",
			resolve: public,
			wantErr: "private, loopback, or link-local",
		},
		{
			name:    "rejects host resolving to private ip",
			input:   "http://internal.example",
			resolve: privateLookup,
			wantErr: "blocked ip",
		},
		{
			name:    "rejects host with any blocked record",
			input:   "http://mixed.example",
			resolve: mixedLookup,
			wantErr: "blocked ip",
		},
		{
			name:    "rejects nxdomain",
			input:   "http://nope.example",
			resolve: errLookup,
			wantErr: "resolve",
		},
		{
			name:    "rejects empty lookup",
			input:   "http://void.example",
			resolve: emptyLookup,
			wantErr: "no addresses",
		},
		{
			name:    "rejects malformed url",
			input:   "ht!tp://%%%%",
			resolve: public,
			wantErr: "parse url",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := validateReferenceURL(tc.input, tc.resolve)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if u.String() != tc.wantFinal {
					t.Fatalf("url mismatch: got %q want %q", u.String(), tc.wantFinal)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"10.1.2.3",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"0.0.0.0",
		"::1",
		"fc00::1",
		"fe80::1",
		"224.0.0.1",
	}
	for _, raw := range blocked {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("could not parse %q", raw)
		}
		if !isBlockedIP(ip) {
			t.Errorf("expected %s to be blocked", raw)
		}
	}

	allowed := []string{
		"93.184.216.34", // example.com
		"8.8.8.8",
		"2606:2800:220:1:248:1893:25c8:1946", // example.com ipv6
	}
	for _, raw := range allowed {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("could not parse %q", raw)
		}
		if isBlockedIP(ip) {
			t.Errorf("expected %s to be allowed", raw)
		}
	}
}
