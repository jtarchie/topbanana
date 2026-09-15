package linkcheck

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

// An offline resolver says "no such host" for every name; that must not cache the whole site as dead.
func TestCheck_NXDomainNeedsWorkingDNS(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		canary error
		want   Status
	}{
		{"dns-up", nil, StatusDead},
		{"dns-down", errors.New("offline"), StatusUnchecked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := "https://invented-" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())) + ".example/"
			c, slug := seed(t, map[string]string{"index.html": `<a href="` + u + `">x</a>`})
			c.client.Transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, &net.DNSError{Err: "no such host", Name: "invented.example", IsNotFound: true}
			}}
			c.canary = func(context.Context) error { return tc.canary }

			rep, err := c.Check(context.Background(), slug)
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Results) != 1 || rep.Results[0].Status != tc.want {
				t.Fatalf("got %+v, want status %s", rep.Results, tc.want)
			}
			_, cached := c.lookup(context.Background(), u)
			if cached != (tc.canary == nil) {
				t.Errorf("cached = %v; an answer given while DNS is down must not be cached", cached)
			}
		})
	}
}
