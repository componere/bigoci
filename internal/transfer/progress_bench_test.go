package transfer_test

import (
	"testing"

	"github.com/stretchr/testify/mock"

	filemocks "github.com/componere/bigoci/internal/file/mocks"
	ocimocks "github.com/componere/bigoci/internal/oci/mocks"
	"github.com/componere/bigoci/internal/plan"
	"github.com/componere/bigoci/internal/transfer"
)

// The pairs below measure what watching a whole transfer costs: the same
// push and the same pull, once with a callback installed and once without.
//
//	go test ./internal/transfer -run '^$' -bench Progress -benchmem
//
// The absolute numbers mean nothing on their own — they are dominated by the
// generated mocks — and neither does comparing an unwatched row here against
// the same row on a tree that predates this feature. testify records a stack
// trace on every mock call, so a benchmark's allocation count moves with the
// depth of the call stack: adding two empty frames to a pull's per-part path
// and nothing else costs more allocations in this harness than the whole of
// the progress accounting does. The gate on the claim that an unwatched
// transfer pays nothing is therefore in progress_internal_bench_test.go,
// which measures the added constructs with no mock anywhere near them.

const (
	// benchFileSize is the file the benchmarks move. It is large enough that
	// the per-chunk counting has somewhere to show up and small enough to
	// stay in memory.
	benchFileSize = 1 << 20
	// benchPartSize splits that file into sixteen parts, so the per-part
	// bookkeeping is exercised rather than amortized into one blob.
	benchPartSize plan.PartSize = 64 << 10
)

func BenchmarkPushWithoutProgress(b *testing.B) {
	benchmarkPush(b, nil)
}

func BenchmarkPushWithProgress(b *testing.B) {
	benchmarkPush(b, func(transfer.Snapshot) {})
}

func BenchmarkPullWithoutProgress(b *testing.B) {
	benchmarkPull(b, nil)
}

func BenchmarkPullWithProgress(b *testing.B) {
	benchmarkPull(b, func(transfer.Snapshot) {})
}

// benchmarkPush moves one file into a registry that holds nothing, with the
// progress callback report or without one when it is nil.
func benchmarkPush(b *testing.B, report transfer.Report) {
	b.Helper()

	content := fileContent(benchFileSize)
	blobs, _ := mockBlobs(b, nil)
	source := mockSource(b, content)
	manifests := acceptingManifests(b)

	b.ReportAllocs()

	for b.Loop() {
		if _, err := transfer.Push(b.Context(), transfer.PushSpec{
			Source:    source,
			Blobs:     blobs,
			Manifests: manifests,
			PartSize:  benchPartSize,
			Workers:   4,
			Title:     "model.bin",
			Progress:  report,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// benchmarkPull moves one file out of a registry that serves every part, with
// the progress callback report or without one when it is nil.
//
// The sink reports itself empty however many bytes it holds, so every
// iteration is the cold pull the benchmark means to measure rather than the
// resume the iteration before it would otherwise have left behind.
func benchmarkPull(b *testing.B, report transfer.Report) {
	b.Helper()

	content := fileContent(benchFileSize)
	artifact, body := artifactFor(b, content, "model.bin")

	blobs := mockBlobsServing(b, newBlobStore(artifact.Parts, content))

	manifests := ocimocks.NewMockManifests(b)
	manifests.EXPECT().Get(mock.Anything).Return(body, manifestDescriptor(body), nil).Maybe()

	file := newMemFile(nil)

	sink := filemocks.NewMockSink(b)
	sink.EXPECT().Size().Return(int64(0), nil).Maybe()
	sink.EXPECT().ReadAt(mock.Anything, mock.Anything).RunAndReturn(file.readAt).Maybe()
	sink.EXPECT().Truncate(mock.Anything).RunAndReturn(file.truncate).Maybe()
	sink.EXPECT().WriteAt(mock.Anything, mock.Anything).RunAndReturn(file.writeAt).Maybe()
	sink.EXPECT().Commit().RunAndReturn(file.commit).Maybe()

	b.ReportAllocs()

	for b.Loop() {
		if err := transfer.Pull(b.Context(), transfer.PullSpec{
			Sink:      sink,
			Blobs:     blobs,
			Manifests: manifests,
			Workers:   4,
			Progress:  report,
		}); err != nil {
			b.Fatal(err)
		}
	}
}
