package bigoci_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci"
	"github.com/componere/bigoci/internal/file"
)

// absentTag is a tag no fixture ever writes.
const absentTag = "absent"

func TestPullReportsAManifestTheRegistryDoesNotHold(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	dest := newPath(t, destName)

	err := newClient(t, bigoci.WithPlainHTTP()).Pull(
		t.Context(),
		reg.taggedRef(absentTag),
		bigoci.ToFile(dest),
	)

	require.ErrorIs(t, err, bigoci.ErrNotFound)
	assert.NoFileExists(t, dest)
	assert.NoFileExists(
		t, dest+file.PartialSuffix,
		"a lookup that moved no bytes must not litter the destination directory",
	)
}

func TestPullReportsAPartTheRegistryLost(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	seedArtifact(t, reg)
	reg.dropBlob(t, reg.artifact(t).Parts[0].Digest)

	err := newClient(t, bigoci.WithPlainHTTP()).Pull(
		t.Context(),
		reg.taggedRef(tag),
		bigoci.ToFile(newPath(t, destName)),
	)

	require.ErrorIs(t, err, bigoci.ErrNotFound, "a missing blob is the same not-found case as a missing manifest")
}

func TestPullReportsAManifestThatIsNotABigociArtifact(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	reg.storeManifest(otherArtifact(t))
	dest := newPath(t, destName)

	err := newClient(t, bigoci.WithPlainHTTP()).Pull(
		t.Context(),
		reg.taggedRef(tag),
		bigoci.ToFile(dest),
	)

	require.ErrorIs(t, err, bigoci.ErrNotBigociArtifact)
	assert.NoFileExists(
		t, dest+file.PartialSuffix,
		"a lookup that moved no bytes must not litter the destination directory",
	)
}

func TestPullReportsAPartThatDoesNotMatchItsDigest(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	seedArtifact(t, reg)
	reg.corruptBlob(t, reg.artifact(t).Parts[0].Digest)
	dest := newPath(t, destName)

	err := newClient(t, bigoci.WithPlainHTTP()).Pull(t.Context(), reg.taggedRef(tag), bigoci.ToFile(dest))

	require.ErrorIs(t, err, bigoci.ErrDigestMismatch)
	assert.NoFileExists(t, dest, "a pull that failed verification must publish nothing")
	assert.FileExists(t, dest+file.PartialSuffix, "the partial file stays behind for a later resume")
}

func TestPushReportsASourceFileThatIsNotThere(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)

	_, err := newClient(t, bigoci.WithPlainHTTP()).Push(
		t.Context(),
		reg.taggedRef(tag),
		bigoci.FromFile(newPath(t, sourceName)),
	)

	require.ErrorIs(t, err, os.ErrNotExist)
	requests, _ := reg.counts()
	assert.Zero(t, requests, "a push must read the file before it opens a connection")
}

func TestPullReportsADestinationItCannotWrite(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	seedArtifact(t, reg)
	dest := filepath.Join(newPath(t, "no-such-directory"), destName)

	err := newClient(t, bigoci.WithPlainHTTP()).Pull(t.Context(), reg.taggedRef(tag), bigoci.ToFile(dest))

	require.ErrorIs(t, err, os.ErrNotExist)
	assert.NoFileExists(t, dest)
}

func TestTransfersReportAReferenceTheyCannotParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  bigoci.Reference
	}{
		{name: "an empty reference names nothing", ref: ""},
		{name: "a reference without a registry is not canonical", ref: "team/artifact:" + tag},
		{name: "a reference with neither tag nor digest names no manifest", ref: "registry.example.com/team/artifact"},
		{name: "an uppercase repository is not a legal name", ref: "registry.example.com/Team/artifact:" + tag},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newClient(t, bigoci.WithPlainHTTP())
			source := bigoci.FromFile(newFile(t, payload(shortFile)))

			_, pushErr := client.Push(t.Context(), tt.ref, source)
			pullErr := client.Pull(t.Context(), tt.ref, bigoci.ToFile(newPath(t, destName)))

			require.Error(t, pushErr)
			require.Error(t, pullErr)
		})
	}
}
