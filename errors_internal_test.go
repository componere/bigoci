package bigoci

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/manifest"
	"github.com/componere/bigoci/internal/oci"
	"github.com/componere/bigoci/internal/transfer"
)

func TestClassifyKeepsTheWholeChain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		internal error
		want     error
	}{
		{
			name:     "a registry miss maps to the public not-found sentinel",
			internal: oci.ErrNotFound,
			want:     ErrNotFound,
		},
		{
			name:     "a part the registry refused maps to the public too-large sentinel",
			internal: oci.ErrTooLarge,
			want:     ErrPartTooLarge,
		},
		{
			name:     "an alien artifact maps to the public not-bigoci sentinel",
			internal: manifest.ErrNotBigociArtifact,
			want:     ErrNotBigociArtifact,
		},
		{
			name:     "a failed verification maps to the public mismatch sentinel",
			internal: transfer.ErrDigestMismatch,
			want:     ErrDigestMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wrapped := fmt.Errorf("some operation detail: %w", tt.internal)

			classified := classify(wrapped)

			require.ErrorIs(t, classified, tt.want, "the public sentinel must be attached")
			require.ErrorIs(t, classified, tt.internal, "the internal sentinel must survive classification")
			require.ErrorContains(t, classified, "some operation detail", "the original message must survive")
		})
	}
}

func TestClassifyLeavesUnknownErrorsAlone(t *testing.T) {
	t.Parallel()

	unknown := errors.New("some transport failure")

	require.Same(t, unknown, classify(unknown), "an error matching no sentinel must come back unchanged")
}
