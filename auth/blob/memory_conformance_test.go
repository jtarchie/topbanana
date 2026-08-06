package blob_test

import (
	"testing"

	"github.com/jtarchie/topbanana/auth/blob"
	"github.com/jtarchie/topbanana/auth/blob/blobtest"
)

// Memory has to pass the same suite every other implementation does. It backs
// this library's own tests, so a Memory that is looser than a real object
// store would let the auth tests above it pass while production breaks —
// which is the precise failure the suite exists to catch.
func TestMemory_Conformance(t *testing.T) {
	blobtest.Run(t, func() blob.Blobs { return blob.NewMemory() })
}
