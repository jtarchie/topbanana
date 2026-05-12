package server

import (
	"fmt"
	"net/http"

	"golang.org/x/crypto/bcrypt"

	"github.com/jtarchie/buildabear/internal/build"
)

// hashPassword returns a bcrypt hash of plain. Returns the empty string when
// plain is empty so callers can pass form values through unconditionally.
func hashPassword(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// verifyBasicAuth checks an incoming request against the site's stored
// credentials. Sites with no PasswordHash are always allowed.
func verifyBasicAuth(meta build.SiteMeta, r *http.Request) bool {
	if meta.PasswordHash == "" {
		return true
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	if meta.Username != "" && username != meta.Username {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(meta.PasswordHash), []byte(password)) == nil
}
