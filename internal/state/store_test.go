package state_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/bloomhollow/internal/state"
)

// conformanceTests is the suite every Store implementation has to satisfy.
// New backends inherit this for free; the cost of adding a backend is wiring
// up its constructor here, not re-deriving the contract.
//
//nolint:gocognit,cyclop // five t.Run blocks, each linear; complexity is in the table, not the control flow.
func conformanceTests(t *testing.T, make func() state.Store) {
	t.Run("load missing returns empty snapshot with empty etag", func(t *testing.T) {
		s := make()
		snap, err := s.Load(context.Background(), "missing-slug")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(snap.Data) != 0 {
			t.Fatalf("expected empty data, got: %v", snap.Data)
		}
		if snap.ETag != "" {
			t.Fatalf("expected empty etag, got: %q", snap.ETag)
		}
	})

	t.Run("save then load roundtrips data", func(t *testing.T) {
		s := make()
		snap := state.NewSnapshot()
		snap.Data["name"] = "anna"
		snap.Data["count"] = float64(3)
		err := s.Save(context.Background(), "round", snap)
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if snap.ETag == "" {
			t.Fatal("expected snap.ETag to be set after Save")
		}

		got, err := s.Load(context.Background(), "round")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got.Data["name"] != "anna" {
			t.Fatalf("name: %v", got.Data["name"])
		}
		// JSON round-trip turns numbers into float64 regardless.
		if got.Data["count"] != float64(3) {
			t.Fatalf("count: %v (%T)", got.Data["count"], got.Data["count"])
		}
		if got.ETag != snap.ETag {
			t.Fatalf("etag mismatch: %q vs %q", got.ETag, snap.ETag)
		}
	})

	t.Run("save with stale etag returns ErrConflict", func(t *testing.T) {
		s := make()
		// First write to establish an etag.
		first := state.NewSnapshot()
		first.Data["v"] = "1"
		err := s.Save(context.Background(), "cas", first)
		if err != nil {
			t.Fatalf("first save: %v", err)
		}

		// Second write with a bogus etag must conflict.
		second := state.NewSnapshot()
		second.ETag = `"not-the-real-etag"`
		second.Data["v"] = "2"
		err = s.Save(context.Background(), "cas", second)
		if !errors.Is(err, state.ErrConflict) {
			t.Fatalf("expected ErrConflict, got: %v", err)
		}
	})

	t.Run("first-write with empty etag conflicts if something exists", func(t *testing.T) {
		s := make()
		// Establish a value.
		first := state.NewSnapshot()
		first.Data["v"] = "1"
		err := s.Save(context.Background(), "ifnone", first)
		if err != nil {
			t.Fatalf("first save: %v", err)
		}
		// Try to "create" again with no etag — should be rejected because
		// the object already exists.
		second := state.NewSnapshot()
		second.Data["v"] = "2"
		err = s.Save(context.Background(), "ifnone", second)
		if !errors.Is(err, state.ErrConflict) {
			t.Fatalf("expected ErrConflict on second create, got: %v", err)
		}
	})

	t.Run("save updates etag for subsequent writes", func(t *testing.T) {
		s := make()
		snap := state.NewSnapshot()
		snap.Data["n"] = float64(1)
		err := s.Save(context.Background(), "seq", snap)
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		firstETag := snap.ETag

		snap.Data["n"] = float64(2)
		err = s.Save(context.Background(), "seq", snap)
		if err != nil {
			t.Fatalf("second save: %v", err)
		}
		if snap.ETag == firstETag {
			t.Fatalf("etag did not change: %q", snap.ETag)
		}
	})
}

func TestMemoryConformance(t *testing.T) {
	conformanceTests(t, func() state.Store { return state.NewMemory() })
}

// TestS3Conformance runs the same suite against a real S3-compatible backend.
// Skipped when AWS_ENDPOINT_URL isn't set; CI environments without minio just
// rely on the memory backend.
func TestS3Conformance(t *testing.T) {
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	bucket := os.Getenv("S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run S3 conformance tests")
	}

	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	// Per-test prefix so reruns don't collide.
	prefix := func() string {
		// nanosecond-precision suffix is enough for sequential test runs.
		return "s3conf-" + randSuffix(t)
	}

	conformanceTests(t, func() state.Store {
		// Each call gets a fresh slug so the "missing" / "first-write" cases
		// see a truly empty bucket prefix.
		s := state.NewS3(client, bucket)
		// Reset the namespace by overriding the slug prefix at the *call site*
		// — Store.Load / Save take slug as a parameter, so just hand each
		// subtest a different slug via t.Run prefixing won't work here.
		// Easiest: wrap so every operation uses a fresh subprefix.
		return &prefixedStore{Store: s, prefix: prefix()}
	})
}

// prefixedStore namespaces every slug under a per-construction prefix. Used
// only in S3 conformance tests so reruns don't see stale data.
type prefixedStore struct {
	state.Store
	prefix string
}

func (p *prefixedStore) Load(ctx context.Context, slug string) (*state.Snapshot, error) {
	return p.Store.Load(ctx, p.prefix+"-"+slug) //nolint:wrapcheck
}

func (p *prefixedStore) Save(ctx context.Context, slug string, snap *state.Snapshot) error {
	return p.Store.Save(ctx, p.prefix+"-"+slug, snap) //nolint:wrapcheck
}

// randSuffix is a tiny per-test-run unique string. Time-based is sufficient
// because we never run in parallel against the same bucket prefix.
func randSuffix(t *testing.T) string {
	t.Helper()
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}
