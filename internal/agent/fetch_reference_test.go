package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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
		wantErr   string
		wantFinal string
	}{
		{name: "https public host", input: "https://example.com/path?q=1", resolve: public, wantFinal: "https://example.com/path?q=1"},
		{name: "http public host", input: "http://example.com", resolve: public, wantFinal: "http://example.com"},
		{name: "rejects ftp scheme", input: "ftp://example.com", resolve: public, wantErr: "unsupported scheme"},
		{name: "rejects file scheme", input: "file:///etc/passwd", resolve: public, wantErr: "unsupported scheme"},
		{name: "rejects missing host", input: "https://", resolve: public, wantErr: "missing host"},
		{name: "rejects localhost literal", input: "http://localhost:8080", resolve: public, wantErr: "localhost"},
		{name: "rejects loopback ip literal", input: "http://127.0.0.1", resolve: public, wantErr: "loopback"},
		{name: "rejects ipv6 loopback literal", input: "http://[::1]/", resolve: public, wantErr: "loopback"},
		{name: "rejects private ip literal", input: "http://10.0.0.1", resolve: public, wantErr: "private"},
		{name: "rejects link-local literal", input: "http://169.254.169.254/latest/meta-data/", resolve: public, wantErr: "link-local"},
		{name: "rejects unspecified ip", input: "http://0.0.0.0", resolve: public, wantErr: "private, loopback, or link-local"},
		{name: "rejects host resolving to private ip", input: "http://internal.example", resolve: privateLookup, wantErr: "blocked ip"},
		{name: "rejects host with any blocked record", input: "http://mixed.example", resolve: mixedLookup, wantErr: "blocked ip"},
		{name: "rejects nxdomain", input: "http://nope.example", resolve: errLookup, wantErr: "resolve"},
		{name: "rejects empty lookup", input: "http://void.example", resolve: emptyLookup, wantErr: "no addresses"},
		{name: "rejects malformed url", input: "ht!tp://%%%%", resolve: public, wantErr: "parse url"},
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
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "0.0.0.0", "::1", "fc00::1", "fe80::1", "224.0.0.1",
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
		"93.184.216.34",
		"8.8.8.8",
		"2606:2800:220:1:248:1893:25c8:1946",
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

// allowLoopback flips blockedIPCheck so httptest.Server (which binds 127.0.0.1)
// passes the SSRF guard for the duration of a test.
func allowLoopback(t *testing.T) {
	t.Helper()
	prev := blockedIPCheck
	blockedIPCheck = func(ip net.IP) bool {
		if ip.IsLoopback() {
			return false
		}
		return isBlockedIP(ip)
	}
	t.Cleanup(func() { blockedIPCheck = prev })
}

func TestFetchAndInlineRewritesStylesheets(t *testing.T) {
	allowLoopback(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/a.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte("body  {  color :  red  ;  }\n"))
	})
	mux.HandleFunc("/b.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte("h1 { font-size: 32px; }"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><head>
<link rel="stylesheet" href="/a.css">
<link rel="stylesheet" href="b.css">
</head><body><h1>hi</h1></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newFetchReferenceClient()
	html, finalURL, truncated, err := fetchAndInline(context.Background(), client, srv.URL+"/")
	if err != nil {
		t.Fatalf("fetchAndInline: %v", err)
	}
	if truncated {
		t.Errorf("did not expect truncation for small fixture")
	}
	if !strings.HasPrefix(finalURL, srv.URL) {
		t.Errorf("finalURL mismatch: got %q", finalURL)
	}
	if strings.Contains(html, `rel="stylesheet"`) {
		t.Errorf("expected <link rel=stylesheet> to be replaced, got:\n%s", html)
	}
	if !strings.Contains(html, "<style>") {
		t.Errorf("expected <style> blocks, got:\n%s", html)
	}
	// Minification should have collapsed the whitespace in a.css.
	if !strings.Contains(html, "body{color:red}") {
		t.Errorf("expected minified a.css rule, got:\n%s", html)
	}
	if !strings.Contains(html, "h1{font-size:32px}") {
		t.Errorf("expected minified b.css rule, got:\n%s", html)
	}
}

func TestFetchAndInlineLeavesBrokenStylesheets(t *testing.T) {
	allowLoopback(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/ok.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte("p { color: blue; }"))
	})
	mux.HandleFunc("/broken.css", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><head>
<link rel="stylesheet" href="/ok.css">
<link rel="stylesheet" href="/broken.css">
</head><body></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newFetchReferenceClient()
	html, _, _, err := fetchAndInline(context.Background(), client, srv.URL+"/")
	if err != nil {
		t.Fatalf("fetchAndInline: %v", err)
	}
	if !strings.Contains(html, "p{color:blue}") {
		t.Errorf("expected /ok.css to be inlined, got:\n%s", html)
	}
	if !strings.Contains(html, `href="/broken.css"`) {
		t.Errorf("expected broken stylesheet <link> to survive, got:\n%s", html)
	}
}

func TestFetchAndInlineRejectsNonHTML(t *testing.T) {
	allowLoopback(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))
	defer srv.Close()

	client := newFetchReferenceClient()
	_, _, _, err := fetchAndInline(context.Background(), client, srv.URL+"/")
	if err == nil || !strings.Contains(err.Error(), "text/html") {
		t.Fatalf("expected text/html guard, got %v", err)
	}
}
