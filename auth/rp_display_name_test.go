package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/auth/blob"
)

// TestAuth_RPDisplayName_ReachesTheCeremony drives the real registerBegin
// handler and asserts the configured display name is what lands in the
// credential-creation options the browser gets — which is the string the
// platform shows in the Face ID / Windows Hello prompt.
//
// Asserted end-to-end rather than by reading cfg back, because the failure
// this guards against is the value being accepted into Config and then not
// threaded into webauthn.Config: a consumer would see its own name in tests
// and another product's name in the actual prompt.
func TestAuth_RPDisplayName_ReachesTheCeremony(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		configure string
		want      string
	}{
		{name: "configured", configure: "Knowhere", want: "Knowhere"},
		{name: "defaulted", configure: "", want: defaultRPDisplayName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			suffix := freshSuffix()
			probeEmail := "probe+" + suffix + "@example.com"

			a, err := New(Config{
				Blobs:           blob.NewMemory(),
				Domain:          "localhost",
				SuperAdminEmail: "super+" + suffix + "@example.com",
				RPDisplayName:   tc.configure,
				InsecureCookies: true,
			})
			if err != nil {
				t.Fatalf("auth.New: %v", err)
			}
			t.Cleanup(func() { _ = a.Close() })

			// Seed the probe user: the library refuses to mint one on
			// registerBegin, same as the cookie-name test.
			probe := &User{Email: probeEmail, Role: RoleAdmin, Created: time.Now().UTC()}
			err = a.Users.Save(context.Background(), probe)
			if err != nil {
				t.Fatalf("seed probe user: %v", err)
			}

			mux := http.NewServeMux()
			a.Passkey.MountRoutes(mux, "/auth/")

			body := strings.NewReader(`{"username":"` + probeEmail + `"}`)
			req := httptest.NewRequest(http.MethodPost, "/auth/passkey/registerBegin", body)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("registerBegin status = %d, body = %s", rec.Code, rec.Body.String())
			}

			var opts struct {
				PublicKey struct {
					RP struct {
						Name string `json:"name"`
						ID   string `json:"id"`
					} `json:"rp"`
				} `json:"publicKey"`
			}
			err = json.Unmarshal(rec.Body.Bytes(), &opts)
			if err != nil {
				t.Fatalf("decode registerBegin body %s: %v", rec.Body.String(), err)
			}
			if opts.PublicKey.RP.Name != tc.want {
				t.Fatalf("rp.name = %q, want %q", opts.PublicKey.RP.Name, tc.want)
			}
		})
	}
}
