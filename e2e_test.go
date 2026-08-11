package bigoci_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/imgoci/bigoci"
	"github.com/imgoci/bigoci/internal/file"
)

// The registry these tests run against: a real zot in a container, which is
// the end-to-end gate the design puts every feature behind.
const (
	// zotImage is the registry image, pinned so a run is reproducible. The tag
	// is a recent one on purpose: v2.1.9 accepts a zero-length blob, answers
	// 201, and then loses it — a later upload of the same digest into another
	// repository deduplicates the first copy away, and the blob starts
	// answering 404. An empty file's single part is exactly that blob, so the
	// empty-file case would pass or fail there depending on timing.
	zotImage = "ghcr.io/project-zot/zot:v2.1.20"
	// zotPort is the port zot serves the distribution API on inside the
	// container.
	zotPort = "5000/tcp"
	// zotConfigPath is the configuration file the image's entrypoint reads.
	zotConfigPath = "/etc/zot/config.json"
	// zotConfigMode is the mode that file is written with: readable by
	// everyone, which is all zot needs.
	zotConfigMode = 0o644
	// zotConfig replaces the image's default configuration with the smallest
	// one that serves the distribution API. The default turns on the search,
	// CVE, and UI extensions, which cost startup time and reach for a
	// vulnerability database these tests have no use for, and it logs at
	// debug level, which buries a real failure.
	zotConfig = `{
  "storage": {"rootDirectory": "/var/lib/registry"},
  "http": {"address": "0.0.0.0", "port": "5000"},
  "log": {"level": "error"}
}`
	// apiPath is the distribution API's base endpoint. A 200 from it is the
	// readiness signal: the registry is answering the protocol, not merely
	// listening.
	apiPath = "/v2/"
)

// The fixture files these tests move, and the part sizes they are split at.
// Part size being an option is what lets a suite that runs on every commit
// exercise the multi-part path at full fidelity without moving gigabytes.
const (
	// largeSize is the length of the large fixture: the 64 MiB the design's
	// per-commit gate names.
	largeSize = 64 << 20
	// largePartSize splits largeSize into largeParts parts of exactly the
	// part size, so the large fixture has no short tail.
	largePartSize bigoci.PartSize = 4 << 20
	// largeParts is how many parts largeSize splits into at largePartSize.
	largeParts = largeSize / int(largePartSize)
	// multiSize is the length of the fixture the tests that only need several
	// parts use. Moving it costs a fraction of the large one.
	multiSize = 1 << 20
	// multiPartSize splits multiSize into multiParts parts.
	multiPartSize bigoci.PartSize = 256 << 10
	// multiParts is how many parts multiSize splits into at multiPartSize.
	multiParts = multiSize / int(multiPartSize)
	// corruptedPart is the part index the corruption fixture rewrites. It is
	// neither the first nor the last, so a pull that fails on it proves the
	// check runs on every part and not just at the edges.
	corruptedPart = 2
	// smallSize is a fixture shorter than any part size here, which splits
	// into a single part.
	smallSize = 100
	// randomSeed seeds the generator every fixture's bytes come from. It is
	// fixed and logged, so a failure is reproducible byte for byte.
	randomSeed uint64 = 20260807
)

// The format contract, spelled out instead of read from the manifest package.
// These are the values docs/docs/reference/format.md publishes, and an
// end-to-end run is where bigoci proves the bytes it left in a registry match
// them rather than proving the encoder agrees with itself.
const (
	// formatArtifactType is the artifactType a bigoci manifest carries.
	formatArtifactType = "application/vnd.bigoci.file.v1"
	// formatPartType is the media type of a part layer.
	formatPartType = "application/vnd.bigoci.file.part.v1"
	// formatConfigType is the media type of the OCI empty descriptor the
	// manifest carries as its config.
	formatConfigType = "application/vnd.oci.empty.v1+json"
	// formatConfigDigest is that descriptor's digest.
	formatConfigDigest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	// formatConfigContent is the two bytes it addresses.
	formatConfigContent = "{}"
	// formatFileDigestKey holds the sha256 of the complete file.
	formatFileDigestKey = "io.bigoci.file.digest"
	// formatFileSizeKey holds the file's size in bytes, as a decimal string.
	formatFileSizeKey = "io.bigoci.file.size"
	// formatPartSizeKey holds the part size in bytes, as a decimal string.
	formatPartSizeKey = "io.bigoci.part.size"
	// formatTitleKey holds the file name recorded at push time.
	formatTitleKey = "org.opencontainers.image.title"
)

// The raw distribution-spec pieces the tests that talk to zot themselves
// need. bigoci's own adapter is the code under test here, so the fixtures
// that check its output, and the ones that write an artifact it would never
// write, speak the protocol directly.
const (
	// uploadsPath is the endpoint suffix that opens a blob upload session.
	uploadsPath = "blobs/uploads/"
	// digestParam names the blob an upload session completes into.
	digestParam = "digest"
	// blobMediaType is the content type a blob upload declares.
	blobMediaType = "application/octet-stream"
	// errorBodyLimit caps how much of an unexpected response body a failure
	// message quotes.
	errorBodyLimit = 4096
	// decimalBase is the base the size annotations are written in.
	decimalBase = 10
)

func TestE2EFilesRoundTripThroughARealRegistry(t *testing.T) {
	reg := newZot(t)
	client := newClient(t, bigoci.WithPlainHTTP())

	t.Run("a large file round trips and lands in the published format", func(t *testing.T) {
		const repo = "e2e/large"

		source := newRandomFile(t, largeSize)
		want := fileDigest(t, source)
		dest := newPath(t, destName)

		desc, err := client.Push(
			t.Context(), reg.taggedRef(repo, tag), bigoci.FromFile(source), bigoci.WithPartSize(largePartSize),
		)
		require.NoError(t, err)
		require.NoError(t, client.Pull(t.Context(), reg.taggedRef(repo, tag), bigoci.ToFile(dest)))

		assert.Equal(t, want, fileDigest(t, dest), "the pulled file must be byte-identical to the pushed one")
		assert.NoFileExists(t, dest+file.PartialSuffix, "a pull that committed leaves no partial file")

		body := reg.rawManifest(t, repo, tag)
		assert.Equal(t, digest.FromBytes(body), desc.Digest, "the descriptor must name the manifest zot holds")
		assertFormat(t, body, artifactFormat{
			fileDigest:   want,
			fileSize:     largeSize,
			partSize:     largePartSize,
			title:        sourceName,
			parts:        largeParts,
			lastPartSize: int64(largePartSize),
		})
	})

	t.Run("the same file always makes the same artifact", func(t *testing.T) {
		const repo = "e2e/determinism"
		const otherTag = "v2"

		source := bigoci.FromFile(newRandomFile(t, multiSize))
		push := func(target string) ocispec.Descriptor {
			desc, err := client.Push(
				t.Context(), reg.taggedRef(repo, target), source, bigoci.WithPartSize(multiPartSize),
			)
			require.NoError(t, err)

			return desc
		}

		first := push(tag)
		again := push(tag)
		elsewhere := push(otherTag)

		assert.Equal(t, first, again, "re-pushing a file the registry already holds must reproduce the manifest")
		assert.Equal(t, first.Digest, elsewhere.Digest, "the tag an artifact is written at is not part of it")
	})

	t.Run("a digest reference pulls the same bytes without a tag", func(t *testing.T) {
		const repo = "e2e/digest"

		source := newRandomFile(t, multiSize)
		dest := newPath(t, destName)

		desc, err := client.Push(
			t.Context(), reg.taggedRef(repo, tag), bigoci.FromFile(source), bigoci.WithPartSize(multiPartSize),
		)
		require.NoError(t, err)
		require.NoError(t, client.Pull(t.Context(), reg.digestRef(repo, desc.Digest), bigoci.ToFile(dest)))

		assert.Equal(t, fileDigest(t, source), fileDigest(t, dest))
	})

	t.Run("a file no longer than one part is a single part", func(t *testing.T) {
		tests := []struct {
			name string
			repo string
			size int64
		}{
			{name: "a file shorter than one part", repo: "e2e/small", size: smallSize},
			{name: "an empty file", repo: "e2e/empty", size: 0},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				source := newRandomFile(t, tt.size)
				want := fileDigest(t, source)
				dest := newPath(t, destName)

				_, err := client.Push(
					t.Context(), reg.taggedRef(tt.repo, tag), bigoci.FromFile(source),
					bigoci.WithPartSize(multiPartSize),
				)
				require.NoError(t, err)
				require.NoError(t, client.Pull(t.Context(), reg.taggedRef(tt.repo, tag), bigoci.ToFile(dest)))

				assert.Equal(t, want, fileDigest(t, dest))

				body := reg.rawManifest(t, tt.repo, tag)
				assertFormat(t, body, artifactFormat{
					fileDigest:   want,
					fileSize:     tt.size,
					partSize:     multiPartSize,
					title:        sourceName,
					parts:        1,
					lastPartSize: tt.size,
				})
				assert.Equal(
					t, []digest.Digest{want}, layerDigests(t, body),
					"a file stored as one part is addressed by its own digest",
				)
			})
		}
	})
}

func TestE2ECorruptedPartsFailThePull(t *testing.T) {
	const repo = "e2e/corrupt"

	reg := newZot(t)
	client := newClient(t, bigoci.WithPlainHTTP())
	source := newRandomFile(t, multiSize)
	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(), reg.taggedRef(repo, tag), bigoci.FromFile(source), bigoci.WithPartSize(multiPartSize),
	)
	require.NoError(t, err)

	parts := partDigests(t, reg, repo)
	require.Len(t, parts, multiParts)
	proxy := newCorruptingProxy(t, reg.host, parts[corruptedPart])

	err = client.Pull(t.Context(), proxy.taggedRef(repo, tag), bigoci.ToFile(dest))

	require.ErrorIs(t, err, bigoci.ErrDigestMismatch)
	require.ErrorContains(
		t, err, "part "+strconv.Itoa(corruptedPart),
		"the failure must name the changed part by its safe bounded index",
	)
	assert.NotContains(
		t,
		err.Error(),
		parts[corruptedPart].String(),
		"the manifest-selected digest is not safe log context",
	)
	assert.NoFileExists(t, dest, "a pull that failed verification must publish nothing")
	assert.FileExists(t, dest+file.PartialSuffix, "the partial file stays behind for a later attempt")

	// Those bytes are what the next pull resumes from, so what it costs is
	// exactly the parts the failed pull left wrong — which only the partial
	// file itself can say, the corrupted part among them.
	intact := intactParts(t, dest+file.PartialSuffix, parts, int64(multiPartSize))
	require.NotContains(t, intact, parts[corruptedPart], "the part that was changed cannot have landed intact")
	require.NotEmpty(t, intact, "every part re-fetched would make this row prove nothing about resume")

	counted := newCountProxy(t, reg.host, proxyDamage{})

	require.NoError(
		t, client.Pull(t.Context(), counted.taggedRef(repo), bigoci.ToFile(dest)),
		"a partial file left by a failed pull must not poison the next one",
	)
	counted.settle(t)
	assertFetched(t, counted.digestsOf(classBlobGet), missing(parts, intact))
	assert.Equal(t, fileDigest(t, source), fileDigest(t, dest))
}

func TestE2EReportsWhatTheRegistryCannotServe(t *testing.T) {
	reg := newZot(t)
	client := newClient(t, bigoci.WithPlainHTTP())

	t.Run("pulling a tag nothing was ever pushed to reports not found", func(t *testing.T) {
		const repo = "e2e/missing"

		err := client.Pull(t.Context(), reg.taggedRef(repo, tag), bigoci.ToFile(newPath(t, destName)))

		require.ErrorIs(t, err, bigoci.ErrNotFound)
	})

	t.Run("pulling an artifact of another kind reports it is not bigoci", func(t *testing.T) {
		const repo = "e2e/alien"

		config := reg.putBlob(t, repo, []byte(formatConfigContent))
		require.Equal(t, digest.Digest(formatConfigDigest), config)
		reg.putManifest(t, repo, tag, otherArtifact(t))

		err := client.Pull(t.Context(), reg.taggedRef(repo, tag), bigoci.ToFile(newPath(t, destName)))

		require.ErrorIs(t, err, bigoci.ErrNotBigociArtifact)
	})
}

// zot is a registry these tests talk to: the address it answers on, whether
// it is the container itself or something standing in front of it.
type zot struct {
	// host is the "host:port" the registry answers on, which is also the
	// registry half of every reference built from it.
	host string
}

// newZot starts a zot registry in a container and returns it, bound to the
// address the container's port is published on. Any opts are applied after
// the ones every zot in this package needs, which is how the broken-network
// suite puts its registry on a private network.
//
// The container is registered for cleanup before its error is checked, so a
// start that got far enough to create one still tears it down, and the
// address is logged so a failing test says which registry it was talking to.
func newZot(t *testing.T, opts ...testcontainers.ContainerCustomizer) zot {
	t.Helper()

	container, err := testcontainers.Run(t.Context(), zotImage,
		append([]testcontainers.ContainerCustomizer{
			testcontainers.WithExposedPorts(zotPort),
			testcontainers.WithFiles(testcontainers.ContainerFile{
				Reader:            strings.NewReader(zotConfig),
				ContainerFilePath: zotConfigPath,
				FileMode:          zotConfigMode,
			}),
			testcontainers.WithWaitStrategy(wait.ForHTTP(apiPath).WithPort(zotPort)),
		}, opts...)...,
	)
	testcontainers.CleanupContainer(t, container)
	require.NoError(t, err, "start %s", zotImage)

	host, err := container.PortEndpoint(t.Context(), zotPort, "")
	require.NoError(t, err)

	t.Logf("%s is serving the distribution API on %s", zotImage, host)

	return zot{host: host}
}

// taggedRef returns the reference of the artifact repo holds at the tag
// target names.
func (z zot) taggedRef(repo, target string) bigoci.Reference {
	return bigoci.Reference(z.host + "/" + repo + ":" + target)
}

// digestRef returns the tagless reference of the manifest dgst names in repo.
func (z zot) digestRef(repo string, dgst digest.Digest) bigoci.Reference {
	return bigoci.Reference(z.host + "/" + repo + "@" + dgst.String())
}

// rawManifest fetches the manifest at target in repo over plain HTTP.
//
// It deliberately does not go through bigoci's own reader: the format
// assertions have to look at the bytes a third-party tool would see, or they
// only prove that the encoder and the decoder agree.
func (z zot) rawManifest(t *testing.T, repo, target string) []byte {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, z.endpoint(repo, "manifests/"+target), nil)
	require.NoError(t, err)
	req.Header.Set("Accept", ocispec.MediaTypeImageManifest)

	resp := z.send(t, req, http.StatusOK)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return body
}

// putBlob uploads content as a blob of repo with the POST-then-PUT pair the
// distribution spec defines, and returns the digest it was stored under.
//
// The alien-artifact fixture needs it: bigoci will not write a manifest it
// would refuse to read, so a test that wants one writes it itself, and a
// registry rejects a manifest whose config blob it does not hold.
func (z zot) putBlob(t *testing.T, repo string, content []byte) digest.Digest {
	t.Helper()

	open, err := http.NewRequestWithContext(t.Context(), http.MethodPost, z.endpoint(repo, uploadsPath), nil)
	require.NoError(t, err)

	opened := z.send(t, open, http.StatusAccepted)
	defer opened.Body.Close()

	location := opened.Header.Get("Location")
	require.NotEmpty(t, location, "the registry opened an upload but sent no Location header")

	session, err := opened.Request.URL.Parse(location)
	require.NoError(t, err)

	dgst := digest.FromBytes(content)
	query := session.Query()
	query.Set(digestParam, dgst.String())
	session.RawQuery = query.Encode()

	complete, err := http.NewRequestWithContext(
		t.Context(), http.MethodPut, session.String(), bytes.NewReader(content),
	)
	require.NoError(t, err)
	complete.Header.Set("Content-Type", blobMediaType)

	completed := z.send(t, complete, http.StatusCreated)
	defer completed.Body.Close()

	return dgst
}

// putManifest writes body as the manifest at target in repo.
func (z zot) putManifest(t *testing.T, repo, target string, body []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(
		t.Context(), http.MethodPut, z.endpoint(repo, "manifests/"+target), bytes.NewReader(body),
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", ocispec.MediaTypeImageManifest)

	resp := z.send(t, req, http.StatusCreated)
	defer resp.Body.Close()
}

// endpoint returns the URL of one distribution-spec endpoint on this
// registry, where path is the suffix under /v2/<repo>/.
func (z zot) endpoint(repo, path string) string {
	return "http://" + z.host + apiPath + repo + "/" + path
}

// send sends req and returns the response, whose body the caller closes. A
// status other than want ends the test with the registry's own explanation of
// what it disliked.
func (z zot) send(t *testing.T, req *http.Request, want int) *http.Response {
	t.Helper()

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "%s %s against %s", req.Method, req.URL.Path, z.host)

	if resp.StatusCode != want {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		require.NoError(t, resp.Body.Close())
		t.Fatalf("%s %s: got %s, want status %d: %s", req.Method, req.URL, resp.Status, want, detail)
	}

	return resp
}

// artifactFormat is what the format reference requires of one pushed
// artifact's manifest. [assertFormat] checks a manifest against it.
type artifactFormat struct {
	// fileDigest is the sha256 of the whole file the annotations must carry.
	fileDigest digest.Digest
	// fileSize is the length of that file in bytes.
	fileSize int64
	// partSize is the size the push split at.
	partSize bigoci.PartSize
	// title is the file name the title annotation must carry, empty when the
	// manifest must carry none.
	title string
	// parts is how many layers the manifest must list.
	parts int
	// lastPartSize is the length of the final layer. Every earlier layer is
	// exactly partSize bytes, which is the split rule.
	lastPartSize int64
}

// newRandomFile writes size bytes of pseudo-random content to a fresh
// temporary directory under sourceName and returns the path it wrote.
//
// Random content is what makes a misplaced part visible: a file of repeated
// bytes would reassemble correctly even if two parts swapped places. The
// generator is seeded from a fixed constant and the size, and the seed is
// logged, so a failure can be reproduced exactly.
func newRandomFile(t *testing.T, size int64) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), sourceName)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fixturePerm)
	require.NoError(t, err)

	written, err := io.CopyN(f, randomBytes(size), size)
	require.NoError(t, err, "write the %d byte fixture", size)
	require.NoError(t, f.Close())
	require.Equal(t, size, written)

	t.Logf("fixture %s holds %d bytes seeded with %d", path, size, seedOf(size))

	return path
}

// randomBytes returns the generator a fixture of size bytes is filled from.
func randomBytes(size int64) io.Reader {
	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[:], seedOf(size))

	return rand.NewChaCha8(seed)
}

// seedOf returns the seed a fixture of size bytes is generated from. Mixing
// the size in keeps two fixtures of different lengths from sharing a prefix.
func seedOf(size int64) uint64 {
	return randomSeed + uint64(size)
}

// fileDigest returns the sha256 digest of the file at path, streamed rather
// than read into memory: the large fixture is 64 MiB.
func fileDigest(t *testing.T, path string) digest.Digest {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	dgst, err := digest.FromReader(f)
	require.NoError(t, err)

	return dgst
}

// assertFormat checks a raw manifest against the published format contract:
// the artifact type, the empty config descriptor, the part layers, and the
// annotations that describe the stored file.
func assertFormat(t *testing.T, body []byte, want artifactFormat) {
	t.Helper()

	var got ocispec.Manifest
	require.NoError(t, json.Unmarshal(body, &got), "the manifest must be JSON: %s", body)

	assert.Equal(t, schemaVersion, got.SchemaVersion)
	assert.Equal(t, ocispec.MediaTypeImageManifest, got.MediaType)
	assert.Equal(t, formatArtifactType, got.ArtifactType)

	assertEmptyConfig(t, body, got.Config)
	assertLayers(t, got.Layers, want)

	annotations := map[string]string{
		formatFileDigestKey: want.fileDigest.String(),
		formatFileSizeKey:   strconv.FormatInt(want.fileSize, decimalBase),
		formatPartSizeKey:   strconv.FormatInt(int64(want.partSize), decimalBase),
	}
	if want.title != "" {
		annotations[formatTitleKey] = want.title
	}

	assert.Equal(t, annotations, got.Annotations)
}

// assertEmptyConfig checks the config descriptor against the format
// reference, raw JSON included.
//
// The raw check is the point of the second look: the image spec's empty
// descriptor carries its two bytes inline in a "data" member, and a manifest
// that kept it would encode differently from every other conforming writer's
// and break the shared manifest digest the format promises.
func assertEmptyConfig(t *testing.T, body []byte, config ocispec.Descriptor) {
	t.Helper()

	assert.Equal(t, formatConfigType, config.MediaType)
	assert.Equal(t, digest.Digest(formatConfigDigest), config.Digest)
	assert.Equal(t, int64(len(formatConfigContent)), config.Size)

	var raw struct {
		// Config is the config descriptor with its members left unparsed.
		Config map[string]json.RawMessage `json:"config"`
	}
	require.NoError(t, json.Unmarshal(body, &raw))
	assert.NotContains(t, raw.Config, "data", "the config descriptor must carry no inline data")
}

// assertLayers checks the part layers against the split rule: one layer per
// part, each carrying the part media type and no annotations, and every layer
// but the last exactly one part long.
func assertLayers(t *testing.T, layers []ocispec.Descriptor, want artifactFormat) {
	t.Helper()

	require.Len(t, layers, want.parts)

	for i, layer := range layers {
		size := int64(want.partSize)
		if i == len(layers)-1 {
			size = want.lastPartSize
		}

		assert.Equal(t, formatPartType, layer.MediaType, "layer %d", i)
		assert.Equal(t, size, layer.Size, "layer %d", i)
		assert.Empty(t, layer.Annotations, "layer %d: parts carry no annotations", i)
	}
}

// layerDigests returns the part digests a raw manifest lists, in file order.
func layerDigests(t *testing.T, body []byte) []digest.Digest {
	t.Helper()

	var got ocispec.Manifest
	require.NoError(t, json.Unmarshal(body, &got))

	digests := make([]digest.Digest, len(got.Layers))
	for i, layer := range got.Layers {
		digests[i] = layer.Digest
	}

	return digests
}

// newCorruptingProxy returns a registry address that serves everything
// upstream does, except that the blob target names comes back with one byte
// changed.
//
// Corrupting the response instead of the registry's storage is what makes the
// test honest: zot keeps answering correctly, so the mismatch can only be
// caught by bigoci hashing what it received. To the client the proxy is
// simply a different registry host.
func newCorruptingProxy(t *testing.T, upstream string, target digest.Digest) zot {
	t.Helper()

	origin, err := url.Parse("http://" + upstream)
	require.NoError(t, err)

	proxy := httputil.NewSingleHostReverseProxy(origin)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.Method != http.MethodGet || !strings.HasSuffix(resp.Request.URL.Path, target.String()) {
			return nil
		}

		// Only the one targeted blob is ever buffered, and it is a part of a
		// fixture measured in kilobytes.
		content, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if err := resp.Body.Close(); err != nil {
			return err
		}

		content[0]++
		resp.Body = io.NopCloser(bytes.NewReader(content))

		return nil
	}

	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")
	t.Logf("a proxy on %s is serving %s in front of %s, with part %s corrupted", host, apiPath, upstream, target)

	return zot{host: host}
}
