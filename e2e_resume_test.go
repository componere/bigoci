package bigoci_test

import (
	"crypto/sha256"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"syscall"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci"
	"github.com/imgoci/bigoci/internal/file"
	"github.com/imgoci/bigoci/internal/retry"
)

// The fixture the kill rows move, and the moment they stop it.
const (
	// killSize is the file the kill rows transfer.
	killSize = 8 << 20
	// killPartSize splits it into parts comfortably larger than a socket
	// buffer, so a part that is still in flight when the signal lands is
	// provably incomplete on disk rather than probably incomplete.
	killPartSize bigoci.PartSize = 1 << 20
	// killParts is how many parts killSize splits into at killPartSize.
	killParts = killSize / int(killPartSize)
	// exactWorkers is the worker count an exact row runs its child at. One
	// worker makes the transfer sequential, which is what turns a single
	// request into a statement about every part before it.
	exactWorkers = 1
	// messyWorkers is the worker count a messy row runs its child at: the
	// library's own default, so the row interrupts a transfer shaped like the
	// ones people actually run.
	messyWorkers = 4
	// killAfterParts is how many parts an exact row lets finish before it
	// kills. It is neither the first nor the last, so a rerun that fetched the
	// wrong end of the file could not pass.
	killAfterParts = 4
	// messyKillBytes is how many blob bytes have to have moved before a messy
	// row kills its child, and the extra part above half the fixture is what
	// makes the row's lower guard a theorem. A worker asks for its next part
	// only once the previous one is whole on disk, so no more than
	// messyWorkers parts can be in flight: once (messyWorkers+1) parts' worth
	// of bytes have moved, at least one part has provably landed. The upper
	// half of "some but not all" is not a construction — responses already
	// streaming keep going while the kill lands, so more parts can complete
	// than the count has seen — which is why requirePartway guards it loudly
	// instead of this constant claiming it.
	messyKillBytes = int64(killPartSize) * (messyWorkers + 1)
)

// What the rows that resume from a planted partial plant, and how the rows
// that continue a part break one.
const (
	// cutBytes is how far into one part's body the continuation rows cut the
	// connection: about a third of a part, so the cut always lands inside a
	// body and never at a boundary.
	cutBytes = int64(multiPartSize) / 3
	// wrongSize is the length of the partial the wrong-size row plants. It is
	// not the length the manifest declares, which is the only evidence a
	// resume rests on.
	wrongSize = multiSize / 2
	// midPartOffset is how far into a part the corrupted-partial row changes a
	// byte. Inside rather than at either edge, so a resume that only checked
	// the ends of a range would fail the row.
	midPartOffset = int64(multiPartSize) / 2
	// flipMask is xored into that byte. Xoring changes whatever was there
	// rather than setting a fixed value, so the row cannot pass by accident on
	// a fixture that already held it.
	flipMask = 0xff
	// hashBufferSize is the scratch these helpers hash a file through. The
	// fixtures are megabytes, so nothing here reads a file into memory.
	hashBufferSize = 256 << 10
)

// TestE2EInterruptedTransfersResume kills transfers against a real registry
// and shows that the rerun moves exactly what the interrupted one left behind
// — no more, and never less.
//
// Every row shares one zot, because starting it is the expensive part, and
// every row is stopped by something the traffic itself decided rather than by
// a clock. That is what makes a kill test worth running on every commit: a
// signal scheduled off a timer either lands after the transfer finished or
// never lets it get anywhere, and neither outcome is rare enough to ignore.
func TestE2EInterruptedTransfersResume(t *testing.T) {
	reg := newZot(t)

	t.Run("pulls stopped by a signal", func(t *testing.T) { killedPullRows(t, reg) })
	t.Run("pulls that find a partial file", func(t *testing.T) { partialPullRows(t, reg) })
	t.Run("pulls whose body dies mid-part", func(t *testing.T) { continuedPullRows(t, reg) })
	t.Run("pushes stopped by a signal", func(t *testing.T) { killedPushRows(t, reg) })
}

// killedPullRows stops a pull with a signal and checks what the rerun costs.
//
// The exact row is the sharp one: with one worker the request for a part is
// made only after the part before it is whole on disk, so killing on the fifth
// distinct blob read names the parts that survived rather than guessing at
// them. The messy rows run at the default parallelism, where no request proves
// anything about another, and read what survived off the disk instead.
func killedPullRows(t *testing.T, reg zot) {
	t.Run("a pull killed between parts fetches exactly the parts it never got", func(t *testing.T) {
		const repo = "resume/pull-exact"

		row := seedRow(t, reg, repo, killSize, killPartSize)
		dest := newPath(t, destName)
		partial := dest + file.PartialSuffix

		state, killer := killedPull(t, reg, repo, dest, exactWorkers, killOnDistinctGets(killAfterParts+1))
		killer.assertTriggered(t)
		requireKilled(t, state)

		assert.NoFileExists(t, dest, "a pull that was killed publishes nothing")
		requireFileSize(t, partial, killSize)
		require.Equal(
			t, row.parts[:killAfterParts], intactParts(t, partial, row.parts, int64(killPartSize)),
			"one worker asks for a part only once the part before it is whole, "+
				"so the partial must hold exactly the parts before the one the kill landed on",
		)

		rerun := newCountProxy(t, reg.host, proxyDamage{})
		pullTo(t, rerun.taggedRef(repo), dest)
		rerun.settle(t)

		assert.Equal(
			t, onceEach(row.parts[killAfterParts:]), rerun.digestsOf(classBlobGet),
			"the rerun must read exactly the parts the killed pull never got, once each",
		)
		assert.Equal(t, 1, rerun.countOf(classManifestGet), "a pull reads the manifest once")
		assert.Equal(t, row.want, fileDigest(t, dest), "the resumed file must be byte-identical to the pushed one")
		assert.NoFileExists(t, partial, "a pull that committed leaves no partial file")
	})

	t.Run("a pull killed mid-part fetches exactly what the partial does not hold", func(t *testing.T) {
		const repo = "resume/pull-messy"

		row := seedRow(t, reg, repo, killSize, killPartSize)
		dest := newPath(t, destName)

		state, killer := killedPull(t, reg, repo, dest, messyWorkers, killOnDownstreamBytes(messyKillBytes))
		killer.assertTriggered(t)
		requireKilled(t, state)

		assertResumes(t, reg, repo, row, dest)
	})

	t.Run("a pull interrupted while it runs leaves a partial its rerun finishes", func(t *testing.T) {
		const repo = "resume/pull-graceful"

		row := seedRow(t, reg, repo, killSize, killPartSize)
		dest := newPath(t, destName)

		killer := newCountProxy(t, reg.host, proxyDamage{})
		child := newHelper(t, helperSpec{
			mode:           helperPull,
			ref:            killer.taggedRef(repo),
			path:           dest,
			workers:        messyWorkers,
			catchInterrupt: true,
		})

		killer.arm(t, killPlan{target: child, sig: syscall.SIGINT, when: killOnDownstreamBytes(messyKillBytes)})
		child.start(t)

		state := child.wait(t)
		killer.settle(t)
		killer.assertTriggered(t)
		requireInterrupted(t, state)

		assertResumes(t, reg, repo, row, dest)
	})
}

// partialPullRows starts a pull from a partial file somebody planted, which is
// how the states a kill cannot be scheduled into get tested at all: a partial
// holding every byte, a partial holding one wrong one, and a partial that
// belongs to a different artifact entirely.
func partialPullRows(t *testing.T, reg zot) {
	t.Run("a partial with one byte changed costs exactly one part", func(t *testing.T) {
		const repo = "resume/partial-corrupt"

		row := seedRow(t, reg, repo, multiSize, multiPartSize)
		dest := newPath(t, destName)
		partial := dest + file.PartialSuffix

		pullTo(t, reg.taggedRef(repo, tag), dest)
		require.NoError(t, os.Rename(dest, partial), "put the pulled file back as the partial a rerun resumes from")
		flipByte(t, partial, int64(corruptedPart)*int64(multiPartSize)+midPartOffset)

		again := newCountProxy(t, reg.host, proxyDamage{})
		pullTo(t, again.taggedRef(repo), dest)
		again.settle(t)

		assert.Equal(
			t, onceEach(row.parts[corruptedPart:corruptedPart+1]), again.digestsOf(classBlobGet),
			"only the part the changed byte falls in may be read again",
		)
		assert.Equal(t, row.want, fileDigest(t, dest))
	})

	t.Run("a partial that already holds the whole file costs no blob read at all", func(t *testing.T) {
		const repo = "resume/partial-complete"

		row := seedRow(t, reg, repo, multiSize, multiPartSize)
		dest := newPath(t, destName)
		partial := dest + file.PartialSuffix

		pullTo(t, reg.taggedRef(repo, tag), dest)
		require.NoError(t, os.Rename(dest, partial), "put the pulled file back as the partial a rerun resumes from")

		again := newCountProxy(t, reg.host, proxyDamage{})
		pullTo(t, again.taggedRef(repo), dest)
		again.settle(t)

		assert.Empty(t, again.digestsOf(classBlobGet), "a partial that verifies whole must cost no blob read")
		assert.Equal(t, 1, again.countOf(classManifestGet), "the manifest is the one thing a resume always fetches")
		assert.Equal(t, row.want, fileDigest(t, dest))
		assert.NoFileExists(t, partial, "the resume must commit the partial it verified")
	})

	t.Run("a partial of some other length is refilled from the start", func(t *testing.T) {
		const repo = "resume/partial-wrong-size"

		row := seedRow(t, reg, repo, multiSize, multiPartSize)
		dest := newPath(t, destName)

		plantPartial(t, dest+file.PartialSuffix, wrongSize)

		again := newCountProxy(t, reg.host, proxyDamage{})
		pullTo(t, again.taggedRef(repo), dest)
		again.settle(t)

		assert.Equal(
			t, onceEach(row.parts), again.digestsOf(classBlobGet),
			"a partial whose length belongs to some other artifact buys nothing, so every part is fetched",
		)
		assert.Equal(t, row.want, fileDigest(t, dest))
	})
}

// continuedPullRows cuts one part's body in half on the wire and checks the
// two answers a registry is allowed to give the attempt that carries on from
// there: the range it asked for, or the whole blob again.
func continuedPullRows(t *testing.T, reg zot) {
	t.Run("a part whose body dies mid-stream is continued", func(t *testing.T) {
		const repo = "resume/continued"

		row := seedRow(t, reg, repo, multiSize, multiPartSize)
		dest := newPath(t, destName)

		cut := newCountProxy(t, reg.host, proxyDamage{cutAfter: cutBytes})
		pullTo(t, cut.taggedRef(repo), dest)
		cut.settle(t)

		cut.assertCut(t)
		assertContinued(t, cut.rangedRecords(), int64(multiPartSize))
		assert.Equal(t, row.want, fileDigest(t, dest))
		assert.NoFileExists(t, dest+file.PartialSuffix, "a pull that committed leaves no partial file")
	})

	t.Run("a part is restarted when the registry ignores the range", func(t *testing.T) {
		const repo = "resume/range-ignored"

		row := seedRow(t, reg, repo, multiSize, multiPartSize)
		dest := newPath(t, destName)

		cut := newCountProxy(t, reg.host, proxyDamage{cutAfter: cutBytes, stripRange: true})
		pullTo(t, cut.taggedRef(repo), dest)
		cut.settle(t)

		cut.assertCut(t)
		ranged := cut.rangedRecords()
		require.NotEmpty(t, ranged, "the pull never asked for a byte range, so the fallback was never taken")

		for _, rec := range ranged {
			assert.Equal(
				t, http.StatusOK, rec.status,
				"%s %s: a registry that never sees the Range header answers with the whole blob", rec.method, rec.dgst,
			)
		}

		assert.Equal(t, row.want, fileDigest(t, dest))
	})
}

// killedPushRows stops a push with a signal and checks what the rerun costs.
//
// A killed push writes no manifest — that is the last thing a push does — so
// what landed is asked of the registry directly rather than read off any
// artifact, and what the rerun has to send follows from the answer.
func killedPushRows(t *testing.T, reg zot) {
	t.Run("a push killed between parts uploads exactly the parts that never landed", func(t *testing.T) {
		const repo = "resume/push-exact"

		source := newStampedFile(t, repo, killSize, killPartSize)
		parts := partsOf(t, source, killPartSize)

		state, killer := killedPush(t, reg, repo, source, exactWorkers, killOnUploadOpens(killAfterParts+1))
		killer.assertTriggered(t)
		requireKilled(t, state)

		landed := blobsHeld(t, reg, repo, parts)
		require.Equal(
			t, parts[:killAfterParts], landed,
			"one worker opens a session only once the part before it has landed, "+
				"so the registry must hold exactly the parts before the one the kill landed on",
		)

		uploaded := repush(t, reg, repo, source, parts)

		assert.Equal(
			t, onceEach(missing(parts, landed)), uploaded,
			"the rerun must upload exactly the parts the killed push never landed, once each",
		)
	})

	t.Run("a push killed mid-part uploads only parts the registry did not hold", func(t *testing.T) {
		const repo = "resume/push-messy"

		source := newStampedFile(t, repo, killSize, killPartSize)
		parts := partsOf(t, source, killPartSize)

		state, killer := killedPush(t, reg, repo, source, messyWorkers, killOnUpstreamBytes(messyKillBytes))
		killer.assertTriggered(t)
		requireKilled(t, state)

		landed := blobsHeld(t, reg, repo, parts)
		requirePartway(t, landed, killParts)
		t.Logf("the killed push left %d of the %d parts in the registry", len(landed), killParts)

		sent := slices.Collect(maps.Keys(repush(t, reg, repo, source, parts)))

		// The set is bounded rather than named, and deliberately. A part whose
		// body the proxy had already forwarded whole is still being committed
		// by the registry when the process that asked for it dies, so what
		// landed can still grow by one after the count above — what cannot
		// happen is the rerun sending a part the registry demonstrably already
		// had.
		requirePartway(t, sent, killParts)
		assert.Subset(
			t, missing(parts, landed), sent,
			"the rerun must never send a part the registry already held when the push was killed",
		)
	})
}

// killedPull runs a pull in a child process through a proxy armed to kill it
// the moment when says the causal event happened, and returns how the child
// ended together with the proxy that stopped it.
//
// Nothing may look at the destination until this returns: the child is reaped
// first, so what is on disk afterwards is a result and not a race.
func killedPull(
	t *testing.T,
	reg zot,
	repo, dest string,
	workers int,
	when killTrigger,
) (*os.ProcessState, *countProxy) {
	t.Helper()

	killer := newCountProxy(t, reg.host, proxyDamage{})
	child := newHelper(t, helperSpec{
		mode: helperPull, ref: killer.taggedRef(repo), path: dest, workers: workers,
	})

	killer.arm(t, killPlan{target: child, sig: syscall.SIGKILL, when: when})
	child.start(t)

	state := child.wait(t)
	killer.settle(t)

	return state, killer
}

// killedPush is [killedPull]'s mirror: one push in a child process, stopped by
// a proxy armed against the traffic the push itself makes.
func killedPush(
	t *testing.T,
	reg zot,
	repo, source string,
	workers int,
	when killTrigger,
) (*os.ProcessState, *countProxy) {
	t.Helper()

	killer := newCountProxy(t, reg.host, proxyDamage{})
	child := newHelper(t, helperSpec{
		mode:     helperPush,
		ref:      killer.taggedRef(repo),
		path:     source,
		partSize: killPartSize,
		workers:  workers,
	})

	killer.arm(t, killPlan{target: child, sig: syscall.SIGKILL, when: when})
	child.start(t)

	state := child.wait(t)
	killer.settle(t)

	return state, killer
}

// assertResumes checks what a messy row's rerun costs, against the only
// witness a messy row has: the partial file the interrupted pull left.
//
// The parts that survived are read off the disk rather than inferred from the
// wire, because at full parallelism no request proves anything about another —
// and a guard that some but not all of them survived is what stops a row whose
// signal landed too early or too late from passing quietly.
func assertResumes(t *testing.T, reg zot, repo string, row rowArtifact, dest string) {
	t.Helper()

	partial := dest + file.PartialSuffix

	assert.NoFileExists(t, dest, "an interrupted pull publishes nothing")
	requireFileSize(t, partial, killSize)

	intact := intactParts(t, partial, row.parts, int64(killPartSize))
	requirePartway(t, intact, killParts)
	t.Logf("the interrupted pull left %d of the %d parts on disk", len(intact), killParts)

	rerun := newCountProxy(t, reg.host, proxyDamage{})
	pullTo(t, rerun.taggedRef(repo), dest)
	rerun.settle(t)

	assertFetched(t, rerun.digestsOf(classBlobGet), missing(row.parts, intact))
	assert.Equal(t, row.want, fileDigest(t, dest), "the resumed file must be byte-identical to the pushed one")
	assert.NoFileExists(t, partial, "a pull that committed leaves no partial file")
}

// repush pushes the same file again through a counting proxy, checks
// everything a rerun owes whatever the kill left behind, and returns how many
// times it sent each part so the row can say which ones those had to be.
//
// What is checked here holds for both push rows: every part is checked before
// anything is sent, every session that opens is completed by one PUT, and the
// artifact the rerun finally wrote pulls back as the file it came from.
func repush(t *testing.T, reg zot, repo, source string, parts []digest.Digest) map[digest.Digest]int {
	t.Helper()

	rerun := newCountProxy(t, reg.host, proxyDamage{})
	pushFrom(t, rerun.taggedRef(repo), source, killPartSize)
	rerun.settle(t)

	assert.ElementsMatch(
		t, parts, slices.Collect(maps.Keys(partsIn(rerun.digestsOf(classBlobHead), parts))),
		"a push checks every part before it decides to send it",
	)
	assert.Equal(
		t, rerun.countOf(classUploadOpen), rerun.countOf(classUploadComplete),
		"every upload session a push opens is completed by exactly one PUT",
	)

	dest := newPath(t, destName)
	pullTo(t, reg.taggedRef(repo, tag), dest)
	assert.Equal(t, fileDigest(t, source), fileDigest(t, dest), "the repushed artifact must be the file it came from")

	return partsIn(rerun.digestsOf(classUploadComplete), parts)
}

// assertContinued reports what the registry answered every range request with
// and, only for the ones it honored, checks that the continued read moved less
// than the whole part.
//
// The distribution spec leaves ranged reads optional, so a registry that
// answers one with the whole blob has not misbehaved and the row must not fail
// it: that answer exercises the fallback instead, which is the sibling row's
// subject. What is asserted here is the arithmetic of a honored range, which is
// the only thing a 206 lets anyone claim.
func assertContinued(t *testing.T, ranged []proxyRecord, partSize int64) {
	t.Helper()

	require.NotEmpty(t, ranged, "the pull never asked for a byte range, so nothing was continued")

	for _, rec := range ranged {
		t.Logf("%s %s: the registry answered the range request with %d, carrying %d bytes",
			rec.method, rec.dgst, rec.status, rec.bytes)

		if rec.status == http.StatusPartialContent {
			assert.Less(
				t, rec.bytes, partSize,
				"a honored range must move only the rest of the part, not the whole of it",
			)
		}
	}
}

// assertFetched checks that the blob reads a rerun made name exactly the parts
// want lists, and that no part was read past its own attempt budget.
func assertFetched(t *testing.T, got map[digest.Digest]int, want []digest.Digest) {
	t.Helper()

	assert.ElementsMatch(
		t, want, slices.Collect(maps.Keys(got)),
		"the rerun must read exactly the parts the partial does not already hold",
	)

	for dgst, reads := range got {
		assert.LessOrEqual(t, reads, retry.DefaultAttempts, "part %s was read past its own attempt budget", dgst)
	}
}

// requirePartway checks that an interrupted transfer really did stop part way
// through. A row where everything or nothing landed proved nothing, whatever
// its outcome assertions say.
func requirePartway(t *testing.T, held []digest.Digest, total int) {
	t.Helper()

	require.NotEmpty(t, held, "the signal landed before a single part was in place, so this row proved nothing")
	require.Less(t, len(held), total, "the signal landed after every part was in place, so this row proved nothing")
}

// requireFileSize checks that the file at path is exactly size bytes. A pull
// sizes its partial file up front, so an interrupted one is the full length
// with holes in it rather than a short file.
func requireFileSize(t *testing.T, path string, size int64) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err, "the interrupted transfer left no partial file at %s", path)
	require.Equal(t, size, info.Size(), "a pull sizes its partial file up front, so an interrupted one is full length")
}

// rowArtifact is everything a row needs to know about the artifact it seeded.
type rowArtifact struct {
	// want is the digest of the file the row pushed, which every rerun has to
	// reproduce.
	want digest.Digest
	// parts are the part digests the manifest lists, in file order.
	parts []digest.Digest
}

// seedRow pushes a fixture no other row shares into repo and returns what the
// registry now holds.
//
// The push goes over the address nothing in the row is breaking: a row proves
// something about a pull, and a fixture that arrived through the damage under
// test would be proving something else.
func seedRow(t *testing.T, reg zot, repo string, size int64, partSize bigoci.PartSize) rowArtifact {
	t.Helper()

	source := newStampedFile(t, repo, size, partSize)
	pushFrom(t, reg.taggedRef(repo, tag), source, partSize)

	parts := partDigests(t, reg, repo)
	require.Len(t, parts, int(size/int64(partSize)), "the seeded artifact must have the split the row expects")

	return rowArtifact{want: fileDigest(t, source), parts: parts}
}

// partDigests returns the part digests the artifact repo holds at the
// fixtures' tag lists, in file order: the names a pull asks the registry for,
// read out of the manifest rather than worked out from the file.
func partDigests(t *testing.T, reg zot, repo string) []digest.Digest {
	t.Helper()

	return layerDigests(t, reg.rawManifest(t, repo, tag))
}

// newStampedFile writes the fixture one row moves: size bytes split at
// partSize, with the row's own name written over the first bytes of every
// part.
//
// The whole suite runs against one zot, and zot answers a blob check from any
// repository once it holds the bytes anywhere. Two rows moving the same content
// would therefore find every part already uploaded and prove nothing, and
// [newRandomFile] seeds only from a size. Stamping changes every part digest
// while leaving the split, the part count, and the tail exactly as the shared
// fixture has them.
func newStampedFile(t *testing.T, name string, size int64, partSize bigoci.PartSize) string {
	t.Helper()

	path := newRandomFile(t, size)
	stamp := []byte(digest.FromString(name).Encoded()[:stampLength])

	f, err := os.OpenFile(path, os.O_WRONLY, fixturePerm)
	require.NoError(t, err)

	defer func() { require.NoError(t, f.Close()) }()

	for offset := int64(0); offset < size; offset += int64(partSize) {
		_, err := f.WriteAt(stamp, offset)
		require.NoError(t, err, "stamp %s into the part at offset %d", name, offset)
	}

	return path
}

// plantPartial writes size bytes of content nothing pushed at path, which is
// how the wrong-length row gets a partial file that belongs to no artifact.
func plantPartial(t *testing.T, path string, size int64) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fixturePerm)
	require.NoError(t, err)

	written, err := io.CopyN(f, randomBytes(size), size)
	require.NoError(t, err, "plant a %d byte partial at %s", size, path)
	require.NoError(t, f.Close())
	require.Equal(t, size, written)
}

// flipByte changes the byte at offset in the file at path.
//
// It reads the byte and writes back a value derived from it rather than
// stamping a constant, so the change is a change whatever the fixture happened
// to hold there.
func flipByte(t *testing.T, path string, offset int64) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDWR, fixturePerm)
	require.NoError(t, err)

	defer func() { require.NoError(t, f.Close()) }()

	var b [1]byte

	_, err = f.ReadAt(b[:], offset)
	require.NoError(t, err, "read the byte at offset %d of %s", offset, path)

	b[0] ^= flipMask

	_, err = f.WriteAt(b[:], offset)
	require.NoError(t, err, "change the byte at offset %d of %s", offset, path)
}

// rangeDigests returns the digest of every partSize-long range of the file at
// path, in file order, with a short final range when the length does not
// divide.
func rangeDigests(t *testing.T, path string, partSize int64) []digest.Digest {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)

	defer func() { require.NoError(t, f.Close()) }()

	info, err := f.Stat()
	require.NoError(t, err)

	hasher := sha256.New()
	buf := make([]byte, hashBufferSize)

	var digests []digest.Digest

	for offset := int64(0); offset < info.Size(); offset += partSize {
		hasher.Reset()

		size := min(partSize, info.Size()-offset)

		read, err := io.CopyBuffer(hasher, io.NewSectionReader(f, offset, size), buf)
		require.NoError(t, err, "hash the range at offset %d of %s", offset, path)
		require.Equal(t, size, read)

		digests = append(digests, digest.NewDigest(digest.SHA256, hasher))
	}

	return digests
}

// intactParts returns the parts the file at path already holds: the digests
// that match at the offset they belong at.
//
// This is where a messy row's assertions come from. Which parts an interrupted
// transfer left behind is a fact about the disk, not about the wire, and a row
// that inferred it from request timing would be asserting a race.
func intactParts(t *testing.T, path string, parts []digest.Digest, partSize int64) []digest.Digest {
	t.Helper()

	got := rangeDigests(t, path, partSize)
	require.Len(t, got, len(parts), "%s is not the length the artifact declares", path)

	var intact []digest.Digest

	for i, want := range parts {
		if got[i] == want {
			intact = append(intact, want)
		}
	}

	return intact
}

// partsOf returns the digests the split of the file at source into partSize
// parts produces, in file order.
//
// A killed push writes no manifest, because writing one is the last thing a
// push does, so a push row works out what the artifact would have been from the
// file it was pushing.
func partsOf(t *testing.T, source string, partSize bigoci.PartSize) []digest.Digest {
	t.Helper()

	return rangeDigests(t, source, int64(partSize))
}

// blobsHeld asks the registry itself which of parts it already holds, over a
// connection of the test's own rather than through the proxy the row was
// breaking. What landed is a fact about the registry, and the only honest place
// to ask about it is the registry.
func blobsHeld(t *testing.T, reg zot, repo string, parts []digest.Digest) []digest.Digest {
	t.Helper()

	client := newHTTPClient(t)

	var held []digest.Digest

	for _, dgst := range parts {
		endpoint := reg.endpoint(repo, "blobs/"+dgst.String())

		req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, endpoint, nil)
		require.NoError(t, err)

		resp, err := client.Do(req)
		require.NoError(t, err, "ask %s whether it holds %s", reg.host, dgst)
		require.NoError(t, resp.Body.Close())

		if resp.StatusCode == http.StatusOK {
			held = append(held, dgst)
		}
	}

	return held
}

// pullTo pulls what ref names into dest with a client of this test's own.
func pullTo(t *testing.T, ref bigoci.Reference, dest string) {
	t.Helper()

	require.NoError(t, newClient(t, bigoci.WithPlainHTTP(), bigoci.WithHTTPClient(newHTTPClient(t))).Pull(
		t.Context(), ref, bigoci.ToFile(dest),
	), "pull %s into %s", ref, dest)
}

// pushFrom pushes source to ref with a client of this test's own.
func pushFrom(t *testing.T, ref bigoci.Reference, source string, partSize bigoci.PartSize) {
	t.Helper()

	_, err := newClient(t, bigoci.WithPlainHTTP(), bigoci.WithHTTPClient(newHTTPClient(t))).Push(
		t.Context(), ref, bigoci.FromFile(source), bigoci.WithPartSize(partSize),
	)
	require.NoError(t, err, "push %s to %s", source, ref)
}

// missing returns the digests of all that held does not carry, in file order:
// the parts a rerun still has to move.
func missing(all, held []digest.Digest) []digest.Digest {
	carried := make(map[digest.Digest]struct{}, len(held))
	for _, dgst := range held {
		carried[dgst] = struct{}{}
	}

	var rest []digest.Digest

	for _, dgst := range all {
		if _, ok := carried[dgst]; !ok {
			rest = append(rest, dgst)
		}
	}

	return rest
}

// onceEach returns the request counts a set of digests asked for exactly once
// produces, which is what a rerun that moved nothing twice looks like.
func onceEach(digests []digest.Digest) map[digest.Digest]int {
	counts := make(map[digest.Digest]int, len(digests))
	for _, dgst := range digests {
		counts[dgst] = 1
	}

	return counts
}

// partsIn drops everything from counts that is not one of parts, which is what
// keeps a push row's assertions about its own parts clear of the empty config
// blob every push also checks and may upload.
func partsIn(counts map[digest.Digest]int, parts []digest.Digest) map[digest.Digest]int {
	wanted := make(map[digest.Digest]struct{}, len(parts))
	for _, dgst := range parts {
		wanted[dgst] = struct{}{}
	}

	kept := make(map[digest.Digest]int, len(counts))

	for dgst, n := range counts {
		if _, ok := wanted[dgst]; ok {
			kept[dgst] = n
		}
	}

	return kept
}
