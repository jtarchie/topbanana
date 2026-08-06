package blobs_test

import (
	"testing"

	"github.com/jtarchie/topbanana/auth/blob"
	"github.com/jtarchie/topbanana/auth/blob/blobtest"
	"github.com/jtarchie/topbanana/internal/blobs"
	"github.com/jtarchie/topbanana/internal/storetest"
)

// The auth stack lives in its own module and tests itself against
// blob.Memory, so nothing over there ever touches S3. This is where that debt
// is paid: the adapter binding the contract to this platform's store runs the
// library's own conformance suite, and against a real bucket whenever
// AWS_ENDPOINT_URL + S3_BUCKET are set.
//
// It is not a formality. The suite's missing-key If-Match case exists because
// S3 and Minio answer 404 rather than 412, which an in-memory double will
// never reproduce — the divergence was found exactly this way.
func TestStoreAdapter_Conformance(t *testing.T) {
	blobtest.Run(t, func() blob.Blobs {
		return blobs.FromStore(storetest.New(t, 0))
	})
}
