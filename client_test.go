package bigoci_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci"
	"github.com/imgoci/bigoci/internal/file"
	"github.com/imgoci/bigoci/internal/manifest"
	"github.com/imgoci/bigoci/internal/plan"
)

// otherTitle is the title the option tests set in place of the file name.
const otherTitle = "renamed.bin"

// blobsPerPush is how many blobs a push of payloadSize bytes uploads: one per
// part, plus the empty config blob the manifest references.
const blobsPerPush = payloadParts + 1

func TestPushAndPullRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		size  int
		parts int
	}{
		{name: "an empty file is one empty part", size: 0, parts: 1},
		{name: "a file shorter than one part is one short part", size: shortFile, parts: 1},
		{name: "a whole multiple of the part size has no short tail", size: wholeParts, parts: 3},
		{name: "a file with a remainder has a short final part", size: payloadSize, parts: payloadParts},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := newRegistry(t)
			client := newClient(t, bigoci.WithPlainHTTP())
			content := payload(tt.size)
			dest := newPath(t, destName)

			desc, err := client.Push(
				t.Context(),
				reg.taggedRef(tag),
				bigoci.FromFile(newFile(t, content)),
				bigoci.WithPartSize(testPartSize),
			)
			require.NoError(t, err)
			require.NoError(t, client.Pull(t.Context(), reg.taggedRef(tag), bigoci.ToFile(dest)))

			body := reg.manifestBody(t)
			assert.Equal(t, ocispec.MediaTypeImageManifest, desc.MediaType)
			assert.Equal(t, manifest.ArtifactType, desc.ArtifactType)
			assert.Equal(
				t, digest.FromBytes(body), desc.Digest,
				"the descriptor must name the manifest that was written",
			)
			assert.Equal(t, int64(len(body)), desc.Size)
			assert.Len(t, reg.artifact(t).Parts, tt.parts)

			pulled, err := os.ReadFile(dest)
			require.NoError(t, err)
			require.Len(t, pulled, len(content))
			assert.Equal(
				t, digest.FromBytes(content), digest.FromBytes(pulled),
				"the pulled file must be byte-identical to the pushed one",
			)
			assert.NoFileExists(t, dest+file.PartialSuffix, "a pull that committed leaves no partial file")
			assert.LessOrEqual(
				t, reg.peakTransfers(), bigoci.DefaultWorkers,
				"a transfer given no worker count must not run more parts at once than the default",
			)
		})
	}
}

func TestPullResolvesADigestReference(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	desc := seedArtifact(t, reg)
	dest := newPath(t, destName)

	err := newClient(t, bigoci.WithPlainHTTP()).Pull(t.Context(), reg.digestRef(desc.Digest), bigoci.ToFile(dest))

	require.NoError(t, err)
	pulled, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, digest.FromBytes(payload(payloadSize)), digest.FromBytes(pulled))
}

func TestClientReachesATLSRegistry(t *testing.T) {
	t.Parallel()

	reg := newTLSRegistry(t)
	client := newClient(t, bigoci.WithHTTPClient(reg.client()))

	_, err := client.Push(
		t.Context(),
		reg.taggedRef(tag),
		bigoci.FromFile(newFile(t, payload(payloadSize))),
		bigoci.WithPartSize(testPartSize),
	)

	require.NoError(t, err, "a client built without WithPlainHTTP must talk https")
	requests, _ := reg.counts()
	assert.Positive(t, requests)
}

func TestNewIgnoresANilHTTPClient(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	client := newClient(t, bigoci.WithHTTPClient(nil), bigoci.WithPlainHTTP())

	_, err := client.Push(
		t.Context(),
		reg.taggedRef(tag),
		bigoci.FromFile(newFile(t, payload(payloadSize))),
		bigoci.WithPartSize(testPartSize),
	)

	require.NoError(t, err, "a nil client must leave the default transport in place")
}

// TestNewReadsTheDockerConfiguration pins where a credential source is built
// and what it costs to build a broken one.
//
// [bigoci.New] returns an error for exactly one reason today, and this is it:
// the caller asked for the credentials a login stored, so the configuration is
// read while a client is being built rather than in the middle of a transfer.
// A file nobody wrote is not a failure — that is a machine nobody has logged
// in on — and a file that is not a configuration is, because the alternative
// is transferring without the credentials the caller asked for and failing
// somewhere that does not mention them.
//
// It runs sequentially: DOCKER_CONFIG belongs to the process, not to the test.
func TestNewReadsTheDockerConfiguration(t *testing.T) {
	t.Run("a directory holding no configuration is the anonymous machine", func(t *testing.T) {
		t.Setenv(dockerConfigEnv, t.TempDir())

		client, err := bigoci.New(bigoci.WithDockerCredentials())

		require.NoError(t, err)
		assert.NotNil(t, client)
	})

	t.Run("a configuration that cannot be read fails while the client is built", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, dockerConfigName), []byte("{not json"), fixturePerm))
		t.Setenv(dockerConfigEnv, dir)

		_, err := bigoci.New(bigoci.WithDockerCredentials())

		require.Error(t, err, "a malformed configuration must be reported before any transfer starts")
		assert.Contains(t, err.Error(), dir, "the failure must name the file it could not read")
	})
}

func TestPushSkipsPartsTheRegistryAlreadyHolds(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	client := newClient(t, bigoci.WithPlainHTTP())
	source := bigoci.FromFile(newFile(t, payload(payloadSize)))

	first, err := client.Push(t.Context(), reg.taggedRef(tag), source, bigoci.WithPartSize(testPartSize))
	require.NoError(t, err)
	_, uploaded := reg.counts()

	second, err := client.Push(t.Context(), reg.taggedRef(tag), source, bigoci.WithPartSize(testPartSize))
	require.NoError(t, err)
	_, uploadedAgain := reg.counts()

	assert.Equal(t, blobsPerPush, uploaded, "a first push uploads every part and the config blob")
	assert.Equal(t, uploaded, uploadedAgain, "a re-push uploads nothing the registry already holds")
	assert.Equal(t, first, second, "the same file at the same part size is the same artifact")
}

func TestPushDefaultsThePartSize(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	client := newClient(t, bigoci.WithPlainHTTP())

	_, err := client.Push(
		t.Context(),
		reg.taggedRef(tag),
		bigoci.FromFile(newFile(t, payload(payloadSize))),
	)

	require.NoError(t, err)
	artifact := reg.artifact(t)
	assert.Equal(t, plan.PartSize(bigoci.DefaultPartSize), artifact.PartSize)
	assert.Len(t, artifact.Parts, 1, "a file far under the default part size is a single part")
}

func TestPushRecordsTheFileName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []bigoci.PushOption
		want string
	}{
		{name: "the title defaults to the name of the file pushed", want: sourceName},
		{name: "WithTitle names the artifact something else", opts: withTitle(otherTitle), want: otherTitle},
		{name: "an empty title records no name at all", opts: withTitle(""), want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := newRegistry(t)

			seedArtifact(t, reg, tt.opts...)

			assert.Equal(t, tt.want, reg.artifact(t).Title)
		})
	}
}

// withTitle returns the option list a title fixture pushes with.
func withTitle(title string) []bigoci.PushOption {
	return []bigoci.PushOption{bigoci.WithTitle(title)}
}

func TestPushByDigestAndPullRoundTrip(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	client := newClient(t, bigoci.WithPlainHTTP())
	content := payload(payloadSize)
	dest := newPath(t, destName)
	before := storedTags(reg)

	desc, err := client.PushByDigest(
		t.Context(),
		reg.repoRef(),
		bigoci.FromFile(newFile(t, content)),
		bigoci.WithPartSize(testPartSize),
	)
	require.NoError(t, err)

	body := reg.manifestAt(t, desc.Digest)
	artifact, err := manifest.Decode(body)
	require.NoError(t, err)

	assert.Equal(t, before, storedTags(reg), "a digest push must not create or move a tag")
	assert.Equal(t, ocispec.MediaTypeImageManifest, desc.MediaType)
	assert.Equal(t, manifest.ArtifactType, desc.ArtifactType)
	assert.Equal(t, digest.FromBytes(body), desc.Digest, "the descriptor must name the manifest that was written")
	assert.Equal(t, int64(len(body)), desc.Size)
	assert.Len(t, artifact.Parts, payloadParts)

	config, _ := manifest.EmptyConfig()
	_, hasConfig := reg.blob(config.Digest.String())
	assert.True(t, hasConfig, "the empty config blob the manifest references must exist")
	for i, part := range artifact.Parts {
		_, hasPart := reg.blob(part.Digest.String())
		assert.True(t, hasPart, "part %d must exist in the repository", i)
	}

	require.NoError(t, client.Pull(t.Context(), reg.digestRef(desc.Digest), bigoci.ToFile(dest)))

	pulled, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Len(t, pulled, len(content))
	assert.Equal(
		t, digest.FromBytes(content), digest.FromBytes(pulled),
		"the pulled file must be byte-identical to the pushed one",
	)
	assert.NoFileExists(t, dest+file.PartialSuffix, "a pull that committed leaves no partial file")
}

func TestPushByDigestReportsAReferenceItCannotParse(t *testing.T) {
	t.Parallel()

	manifestDigest := digest.FromBytes(payload(shortFile)).String()
	tests := []struct {
		name    string
		ref     bigoci.Reference
		wantMsg string
	}{
		{name: "an empty reference names nothing", ref: ""},
		{name: "a reference without a registry is not canonical", ref: "team/artifact"},
		{name: "an uppercase repository is not a legal name", ref: "registry.example.com/Team/artifact"},
		{
			name:    "a tagged reference is a bound write",
			ref:     "registry.example.com/team/artifact:" + tag,
			wantMsg: "must name a repository only",
		},
		{
			name:    "a digest reference claims a body that does not exist yet",
			ref:     bigoci.Reference("registry.example.com/team/artifact@" + manifestDigest),
			wantMsg: "must name a repository only",
		},
		{
			name:    "a reference carrying both a tag and a digest is a bound write",
			ref:     bigoci.Reference("registry.example.com/team/artifact:" + tag + "@" + manifestDigest),
			wantMsg: "must name a repository only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := newRegistry(t)
			source := bigoci.FromFile(newFile(t, payload(shortFile)))

			_, err := newClient(t, bigoci.WithPlainHTTP()).PushByDigest(t.Context(), tt.ref, source)

			require.Error(t, err)
			if tt.wantMsg != "" {
				assert.Contains(t, err.Error(), tt.wantMsg)
			}
			requests, _ := reg.counts()
			assert.Zero(t, requests, "a reference that is not one must be reported before any I/O")
		})
	}
}

func TestPushByDigestHonorsTheSameOptionsAsPush(t *testing.T) {
	t.Parallel()

	t.Run("the title defaults to the name of the file pushed", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, sourceName, digestPushedArtifact(t).Title)
	})

	t.Run("WithTitle names the artifact something else", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, otherTitle, digestPushedArtifact(t, bigoci.WithTitle(otherTitle)).Title)
	})

	t.Run("an empty title records no name at all", func(t *testing.T) {
		t.Parallel()

		assert.Empty(t, digestPushedArtifact(t, bigoci.WithTitle("")).Title)
	})

	t.Run("WithPartSize splits the file the same way a tagged push does", func(t *testing.T) {
		t.Parallel()

		artifact := digestPushedArtifact(t, bigoci.WithPartSize(testPartSize))

		assert.Equal(t, plan.PartSize(testPartSize), artifact.PartSize)
		assert.Len(t, artifact.Parts, payloadParts)
	})

	t.Run("the part size defaults", func(t *testing.T) {
		t.Parallel()

		reg := newRegistry(t)

		desc, err := newClient(t, bigoci.WithPlainHTTP()).PushByDigest(
			t.Context(),
			reg.repoRef(),
			bigoci.FromFile(newFile(t, payload(payloadSize))),
		)
		require.NoError(t, err)

		artifact, err := manifest.Decode(reg.manifestAt(t, desc.Digest))
		require.NoError(t, err)
		assert.Equal(t, plan.PartSize(bigoci.DefaultPartSize), artifact.PartSize)
		assert.Len(t, artifact.Parts, 1, "a file far under the default part size is a single part")
	})

	t.Run("WithWorkers limits concurrency", func(t *testing.T) {
		t.Parallel()

		reg := newRegistry(t)

		_, err := newClient(t, bigoci.WithPlainHTTP()).PushByDigest(
			t.Context(),
			reg.repoRef(),
			bigoci.FromFile(newFile(t, payload(payloadSize))),
			bigoci.WithPartSize(testPartSize),
			bigoci.WithWorkers(1),
		)
		require.NoError(t, err)
		assert.Equal(t, 1, reg.peakTransfers(), "one worker moves the parts one at a time")
	})
}

func TestPushByDigestRejectsOptionsItCannotHonor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opt  bigoci.PushOption
	}{
		{name: "a zero part size splits a file into nothing", opt: bigoci.WithPartSize(0)},
		{name: "a negative part size splits a file into nothing", opt: bigoci.WithPartSize(-1)},
		{name: "zero workers move no parts", opt: bigoci.WithWorkers(0)},
		{name: "a negative worker count moves no parts", opt: bigoci.WithWorkers(-1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := newRegistry(t)
			source := bigoci.FromFile(newFile(t, payload(payloadSize)))

			_, err := newClient(t, bigoci.WithPlainHTTP()).PushByDigest(
				t.Context(),
				reg.repoRef(),
				source,
				tt.opt,
			)

			require.Error(t, err)
			requests, _ := reg.counts()
			assert.Zero(t, requests, "unusable options must be reported before any I/O")
		})
	}
}

// digestPushedArtifact pushes the fixture file by digest with the given
// options on top of testPartSize, and returns the decoded artifact.
func digestPushedArtifact(t *testing.T, opts ...bigoci.PushOption) manifest.Artifact {
	t.Helper()

	reg := newRegistry(t)
	desc, err := newClient(t, bigoci.WithPlainHTTP()).PushByDigest(
		t.Context(),
		reg.repoRef(),
		bigoci.FromFile(newFile(t, payload(payloadSize))),
		append([]bigoci.PushOption{bigoci.WithPartSize(testPartSize)}, opts...)...,
	)
	require.NoError(t, err)

	artifact, err := manifest.Decode(reg.manifestAt(t, desc.Digest))
	require.NoError(t, err)

	return artifact
}

// repoRef returns the repository-only reference [bigoci.Client.PushByDigest]
// accepts for this registry.
func (r *registry) repoRef() bigoci.Reference {
	return bigoci.Reference(r.host + "/" + repoName)
}

// manifestAt returns the raw manifest the registry holds at dgst.
func (r *registry) manifestAt(t *testing.T, dgst digest.Digest) []byte {
	t.Helper()

	r.mu.Lock()
	body, ok := r.manifests[dgst.String()]
	r.mu.Unlock()

	require.True(t, ok, "the registry holds no manifest at %s", dgst)

	return body
}

// storedTags returns the tags this registry currently holds, sorted, so a
// digest push can prove it created or moved none.
func storedTags(reg *registry) []string {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	tags := make([]string, 0)
	for key := range reg.manifests {
		if !strings.Contains(key, ":") {
			tags = append(tags, key)
		}
	}
	slices.Sort(tags)

	return tags
}
