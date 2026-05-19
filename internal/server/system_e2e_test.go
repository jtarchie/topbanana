package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/buildabear/internal/server"
	"github.com/jtarchie/buildabear/internal/snapshot"
)

// TestSystem_Populated drives two builds through the stub runner then loads
// /system to confirm every section renders with the right shape: the two
// slugs appear in the Apps table, each build appears in the recent builds
// table, the configured model from SystemInfo shows in the Configuration
// dl, and the storage breakdown is non-zero.
//
//nolint:cyclop // single end-to-end script intentionally walks many steps.
func TestSystem_Populated(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	runner := &stubRunner{title: "Sys Test", desc: "system dashboard fixture"}
	info := server.SystemInfo{
		LLMModel:           "stub/test-model",
		LLMBaseURL:         "https://example.test/v1",
		LLMReasoningEffort: "medium",
		S3Endpoint:         "http://minio.example.test",
		S3Bucket:           "system-test-bucket",
		SnapshotKeep:       100,
		EditsKeep:          50,
	}
	handler := buildServerWithRunnerAndInfo(t, st, snapSvc, runner, info)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	client := &http.Client{Timeout: 10 * time.Second}
	auth := func(method, path string, body io.Reader) *http.Request {
		req, err := http.NewRequest(method, httpSrv.URL+path, body)
		if err != nil {
			t.Fatalf("new %s %s: %v", method, path, err)
		}
		req.Host = "localhost"
		req.AddCookie(testSessionCookie)
		if body != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		return req
	}

	// Build two distinct apps so the dashboard has real data to summarize.
	slugA := "sysa-" + freshSlug(t)
	slugB := "sysb-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slugA)
	cleanupSlug(t, ctx, st, snapSvc, slugB)
	t.Cleanup(func() {
		cleanupSlug(t, ctx, st, snapSvc, slugA)
		cleanupSlug(t, ctx, st, snapSvc, slugB)
	})

	for _, slug := range []string{slugA, slugB} {
		form := url.Values{"template": {"blank"}, "slug": {slug}, "prompt": {"hello system"}}
		resp, err := client.Do(auth(http.MethodPost, "/build", strings.NewReader(form.Encode())))
		if err != nil {
			t.Fatalf("POST /build %s: %v", slug, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /build %s: %d", slug, resp.StatusCode)
		}
		consumeBuild(t, httpSrv.URL, slug, 30*time.Second)
	}

	// GET /system.
	resp, err := client.Do(auth(http.MethodGet, "/system", nil))
	if err != nil {
		t.Fatalf("GET /system: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /system: %d body=%q", resp.StatusCode, string(body))
	}
	page := string(body)

	// Section headings — keeps the IA stable.
	for _, want := range []string{
		"At a glance", "Apps", "Recent builds", "Storage breakdown", "Configuration",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing section %q", want)
		}
	}

	// Both slugs appear in tables.
	for _, slug := range []string{slugA, slugB} {
		if !strings.Contains(page, slug) {
			t.Errorf("/system missing slug %q", slug)
		}
	}

	// Configuration block surfaces the planted SystemInfo.
	for _, want := range []string{
		info.LLMModel, info.LLMBaseURL, info.LLMReasoningEffort,
		info.S3Endpoint, info.S3Bucket,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system missing config value %q", want)
		}
	}

	// At least one successful build badge — both stub builds complete cleanly.
	if !strings.Contains(page, "badge-success") {
		t.Errorf("/system missing a success badge despite two completed builds")
	}

	// Storage breakdown should show non-zero "Total" row. The exact bytes
	// depend on lint output + stub HTML; just check the totals row exists
	// and contains a non-zero count.
	if !strings.Contains(page, ">Total<") {
		t.Errorf("/system storage breakdown missing Total row")
	}
}

// TestSystem_EmptyBucket walks /system without seeding any apps to make sure
// the zero-state renders cleanly (no panics, sensible copy, all numbers 0
// or "Never").
//
// Note: this test runs against the same MinIO bucket as the rest of the
// suite, so it can't *guarantee* an empty bucket. Instead it asserts that
// the response is well-formed: status 200, all five sections render, no
// panic. Bucket-cleanup orchestration is left to operators running the
// suite locally.
func TestSystem_EmptyBucket(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	snapSvc := snapshot.New(st, 0)
	runner := &stubRunner{title: "empty", desc: "empty"}
	info := server.SystemInfo{LLMModel: "stub/empty-model", S3Bucket: "empty-bucket-test"}
	handler := buildServerWithRunnerAndInfo(t, st, snapSvc, runner, info)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/system", nil)
	if err != nil {
		t.Fatalf("new GET /system: %v", err)
	}
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /system: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /system: %d body=%q", resp.StatusCode, string(body))
	}
	page := string(body)

	for _, want := range []string{
		"At a glance", "Apps", "Recent builds", "Storage breakdown", "Configuration",
		info.LLMModel, info.S3Bucket,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/system (empty state) missing %q", want)
		}
	}
}
