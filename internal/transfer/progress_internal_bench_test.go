package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"testing"

	digest "github.com/opencontainers/go-digest"

	"github.com/imgoci/bigoci/internal/retry"
)

// This file is the gate on the claim that a transfer nobody is watching pays
// nothing for the accounting. It measures the added constructs directly, one
// at a time, because the whole-transfer benchmarks next door cannot: those run
// against generated mocks, and testify records a stack trace on every mock
// call, so the two extra frames the retry wrapper adds cost more allocations
// in the harness than the entire feature costs in the code. Measured here, on
// the same tree, the unwatched accounting rows must report zero allocations.
// The source-read row is different: each iteration hashes the payload and
// compares the digest, and those allocations are the cost the row reports.
//
//	go test ./internal/transfer -run '^$' -bench Unwatched -benchmem

// benchPayload is the chunk the reader benchmarks hand over, one part of a
// copy buffer's worth.
const benchPayload = 64 << 10

func BenchmarkAttemptedUnwatched(b *testing.B) {
	benchmarkAttempted(b, nil)
}

func BenchmarkAttemptedWatched(b *testing.B) {
	benchmarkAttempted(b, newReporter(func(Snapshot) {}))
}

func BenchmarkSourceReadsUnwatched(b *testing.B) {
	benchmarkSourceReads(b, nil)
}

func BenchmarkSourceReadsWatched(b *testing.B) {
	benchmarkSourceReads(b, newReporter(func(Snapshot) {}))
}

func BenchmarkHashWritersUnwatched(b *testing.B) {
	benchmarkHashWriters(b, nil)
}

func BenchmarkHashWritersWatched(b *testing.B) {
	benchmarkHashWriters(b, newReporter(func(Snapshot) {}))
}

// benchmarkAttempted measures the retry wrapper at the shape upload calls it
// with: one attempt that succeeds, and a closure over the flag the skip rule
// reads.
func benchmarkAttempted(b *testing.B, report *reporter) {
	b.Helper()

	ctx := b.Context()
	policy := retry.Policy{Attempts: 1}

	b.ReportAllocs()

	for b.Loop() {
		var uploaded bool

		err := attempted(ctx, report, policy, func(context.Context) error {
			uploaded = true

			return nil
		})
		if err != nil || !uploaded {
			b.Fatal("the attempt must run exactly once and succeed")
		}
	}
}

// benchmarkSourceReads measures the source-error tagging reader on the upload
// read path, including the per-part hasher that catches a same-length
// mutation when a Content-Length transport never reads EOF.
func benchmarkSourceReads(b *testing.B, _ *reporter) {
	b.Helper()

	payload := make([]byte, benchPayload)
	into := make([]byte, benchPayload)
	want := digest.FromBytes(payload)
	source := bytes.NewReader(payload)

	b.ReportAllocs()

	for b.Loop() {
		source.Reset(payload)
		reader := tagSourceReads{
			r:      source,
			hasher: sha256.New(),
			expect: want,
			size:   int64(len(payload)),
		}

		n, err := reader.Read(into)
		if n != len(payload) {
			b.Fatalf("read %d bytes of a %d-byte payload", n, len(payload))
		}
		if err != nil && err != io.EOF {
			b.Fatal(err)
		}
	}
}

// benchmarkHashWriters measures the writer a local hash pass copies through,
// which is where the third writer is installed or not installed at all.
func benchmarkHashWriters(b *testing.B, report *reporter) {
	b.Helper()

	hashers := io.MultiWriter(sha256.New(), sha256.New())
	chunk := make([]byte, benchPayload)

	b.ReportAllocs()

	var into io.Writer
	for b.Loop() {
		into = hashesInto(hashers, report)
	}

	if _, err := into.Write(chunk); err != nil {
		b.Fatal(err)
	}
}
