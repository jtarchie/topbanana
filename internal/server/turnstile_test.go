package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/sandbox"
)

func TestCheckTurnstile(t *testing.T) {
	var gotSecret, gotToken string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotSecret, gotToken = r.PostForm.Get("secret"), r.PostForm.Get("response")
		if gotToken == "good" {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	}))
	t.Cleanup(fake.Close)
	orig := turnstileVerifyURL
	turnstileVerifyURL = fake.URL
	t.Cleanup(func() { turnstileVerifyURL = orig })

	status := func(err error) int {
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return he.Code
		}
		return 0
	}
	ctx := context.Background()

	missing := sandbox.Request{Form: map[string]string{"email": "a@b.c"}}
	err := checkTurnstile(ctx, "s3cret", &missing)
	if status(err) != http.StatusForbidden {
		t.Fatalf("missing token: got %v, want 403", err)
	}

	bad := sandbox.Request{Form: map[string]string{turnstileField: "bad"}}
	err = checkTurnstile(ctx, "s3cret", &bad)
	if status(err) != http.StatusForbidden {
		t.Fatalf("rejected token: got %v, want 403", err)
	}

	form := sandbox.Request{Form: map[string]string{"email": "a@b.c", turnstileField: "good"}}
	err = checkTurnstile(ctx, "s3cret", &form)
	if err != nil {
		t.Fatalf("valid form token: %v", err)
	}
	if gotSecret != "s3cret" {
		t.Fatalf("secret sent = %q", gotSecret)
	}
	if _, ok := form.Form[turnstileField]; ok || form.Form["email"] != "a@b.c" {
		t.Fatalf("token should be stripped, other fields kept: %v", form.Form)
	}

	js := sandbox.Request{JSON: map[string]any{"email": "a@b.c", turnstileField: "good"}}
	err = checkTurnstile(ctx, "s3cret", &js)
	if err != nil {
		t.Fatalf("valid json token: %v", err)
	}
	if _, ok := js.JSON.(map[string]any)[turnstileField]; ok {
		t.Fatalf("json token should be stripped: %v", js.JSON)
	}
}
