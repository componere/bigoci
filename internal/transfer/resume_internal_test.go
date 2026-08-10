package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContextReaderPreservesContextErrors pins both reasons a caller can end
// a resume hash. The surrounding pull may wrap either error with part context,
// but [errors.Is] must still distinguish cancellation from a deadline.
func TestContextReaderPreservesContextErrors(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	deadline, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	t.Cleanup(stop)

	tests := []struct {
		// name describes the way the context ended.
		name string
		// ctx is already ended when the reader sees it.
		ctx context.Context
		// want is the reason callers must still reach through wrapping.
		want error
	}{
		{name: "cancelled", ctx: cancelled, want: context.Canceled},
		{name: "deadline", ctx: deadline, want: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			read, err := (contextReader{ctx: tt.ctx, reader: bytes.NewReader([]byte("unread"))}).Read(make([]byte, 1))
			require.ErrorIs(t, err, tt.want)
			assert.Zero(t, read, "an ended context prevents another disk read")
		})
	}
}

// BenchmarkResumeHashContextCheck compares the resume hash's cancellation
// check with the same buffered hash pass without it. The file lives in memory
// so the result isolates hashing and the per-buffer context checks from disk
// variance.
func BenchmarkResumeHashContextCheck(b *testing.B) {
	const size = 32 << 20

	content := make([]byte, size)
	tests := []struct {
		// name becomes the benchmark's mode column.
		name string
		// wrap adds the behavior the row measures to a fresh section reader.
		wrap func(io.Reader) io.Reader
	}{
		{
			name: "mode=direct",
			wrap: func(reader io.Reader) io.Reader {
				return reader
			},
		},
		{
			name: "mode=context",
			wrap: func(reader io.Reader) io.Reader {
				return contextReader{ctx: context.Background(), reader: reader}
			},
		},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			hasher := sha256.New()
			buf := make([]byte, copyBufferSize)
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				hasher.Reset()
				section := io.NewSectionReader(bytes.NewReader(content), 0, size)
				read, err := io.CopyBuffer(hasher, tt.wrap(section), buf)
				if err != nil {
					b.Fatal(err)
				}
				if read != size {
					b.Fatalf("hashed %d bytes, want %d", read, size)
				}
			}
		})
	}
}
