package server

// Preview tokens let a PRIVATE site's subresources load inside the canvas.
// The canvas serves site HTML with a CSP sandbox (opaque origin), so the
// document's css/image fetches carry no cookies and the private gate would
// 404 them. The canvas instead mounts the site at /sp/{slug}/{token}/…: the
// token — an HMAC over slug+expiry with a per-process random key — stands in
// for the session on that slug's static reads only.
//
// Scope of what a leaked token grants: read access to that one site's files
// for tokenTTL. The token is only ever embedded into pages served to someone
// who already passed canEdit, and the only scripts that could exfiltrate it
// are the site's own — which already contain the content it unlocks.
//
// ponytail: the key is per-process, so tokens die on deploy/restart and (if
// the app ever scales out) aren't valid across instances — the canvas just
// needs a reload. Upgrade path: derive the key from a configured secret.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

const previewTokenTTL = time.Hour

var previewTokenKey = func() []byte {
	k := make([]byte, 32)
	_, err := rand.Read(k)
	if err != nil {
		panic("preview token key: " + err.Error()) // rand.Read failing means no crypto at all
	}
	return k
}()

func previewTokenMAC(slug string, exp int64) string {
	mac := hmac.New(sha256.New, previewTokenKey)
	mac.Write([]byte(slug + "|" + strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// mintPreviewToken returns a token authorizing static reads of slug until the
// TTL passes. Format: {expiry-unix}.{hex-hmac} — URL-safe, no padding.
func mintPreviewToken(slug string) string {
	exp := time.Now().Add(previewTokenTTL).Unix()
	return strconv.FormatInt(exp, 10) + "." + previewTokenMAC(slug, exp)
}

// validPreviewToken reports whether tok authorizes slug right now.
func validPreviewToken(slug, tok string) bool {
	expStr, macHex, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	want := previewTokenMAC(slug, exp)
	return hmac.Equal([]byte(macHex), []byte(want))
}
