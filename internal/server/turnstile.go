package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/sandbox"
)

// Hidden input the widget injects into its <form>; JSON posts must use the same key.
const turnstileField = "cf-turnstile-response"

// turnstileVerifyURL is a var so tests can point it at an httptest server.
var turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

var turnstileClient = &http.Client{Timeout: 10 * time.Second}

// Server-side because a bot POSTs straight to /api/* and never renders the widget.
func checkTurnstile(ctx context.Context, secret string, req *sandbox.Request) error {
	token := req.Form[turnstileField]
	obj, isObj := req.JSON.(map[string]any)
	if token == "" && isObj {
		token, _ = obj[turnstileField].(string)
	}
	if token == "" {
		return echo.NewHTTPError(http.StatusForbidden, "verification required: complete the challenge and submit again")
	}

	ok, err := verifyTurnstileToken(ctx, secret, token)
	if err != nil {
		// Fail closed: the visitor sees an error and can retry, nothing is lost.
		return httpErr(http.StatusBadGateway, "turnstile verify", err)
	}
	if !ok {
		return echo.NewHTTPError(http.StatusForbidden, "verification failed: reload the page and try again")
	}

	// Handlers that persist the whole form shouldn't store a 2 KB token per row.
	delete(req.Form, turnstileField)
	if isObj {
		delete(obj, turnstileField)
	}
	return nil
}

// remoteip is omitted: behind a proxied custom domain RealIP is a Cloudflare edge address.
func verifyTurnstileToken(ctx context.Context, secret, token string) (bool, error) {
	form := url.Values{"secret": {secret}, "response": {token}}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileVerifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := turnstileClient.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("siteverify: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("siteverify: status %d", resp.StatusCode)
	}

	var out struct {
		Success bool `json:"success"`
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	if err != nil {
		return false, fmt.Errorf("decode siteverify: %w", err)
	}
	return out.Success, nil
}
