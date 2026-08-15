package oci

import (
	"bytes"
	"context"

	// Register sha256 with go-digest. Both directions digest the manifest
	// bytes with the canonical algorithm, whose hash panics when it is not
	// linked — the tests cannot catch its absence because the testing
	// framework links it for them.
	_ "crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/imgoci/bigoci/internal/retry"
)

// maxManifestSize is the largest manifest body this adapter reads. Registries
// commonly cap manifests at 4 MiB, and the format's own part cap keeps a
// bigoci manifest around 800 KB, so a larger body means something other than
// a manifest is answering.
const maxManifestSize = 4 << 20

// Manifests reads and writes the manifest of one repository. It implements
// the transfer package's Manifests port and comes from [Repository.Manifests].
//
// How the endpoints are addressed is fixed when the repository is built
// rather than passed on every call, which keeps the reference grammar out of
// the core: the core asks for "the manifest" and this adapter knows which
// one that is. A repository built by [NewRepository] is bound to the tag or
// digest the reference carried. A repository built by
// [NewDigestPushRepository] publishes at the digest of the body Put is given
// and has no bound fetch target.
type Manifests struct {
	// repo is the repository whose manifest endpoints this adapter talks to,
	// and which carries either the tag or digest they address or the digest
	// publication mode Put uses.
	repo *Repository
}

// Get fetches the manifest the bound reference resolves to and returns its
// raw bytes with a descriptor for them.
//
// The descriptor's digest is computed from the bytes that arrived. Registries
// also send the digest in a header, but a digest a caller verifies content
// against cannot come from the same response the content did. When the
// repository was built from a digest reference, Get also checks the bytes
// against that digest and fails when they disagree.
//
// The body is read under a 4 MiB limit rather than into whatever the far end
// sends, and a manifest above the limit is an error. The media type is the
// response's Content-Type and the size is the length of the returned bytes.
//
// A reference the registry does not resolve is an error wrapping
// [ErrNotFound].
//
// A repository built by [NewDigestPushRepository] has no bound tag or
// digest to fetch. Get is a misuse of that repository and fails without
// talking to the registry.
func (m *Manifests) Get(ctx context.Context) ([]byte, ocispec.Descriptor, error) {
	if m.repo.manifest.publishByDigest {
		return nil, ocispec.Descriptor{}, errors.New("digest-push repository has no bound manifest to fetch")
	}

	endpoint := m.repo.endpoint(manifestPath(m.repo.manifest.path))

	req, err := m.repo.newRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ocispec.Descriptor{}, err
	}
	req.Header.Set("Accept", ocispec.MediaTypeImageManifest)

	at := originOf(req)

	resp, err := m.repo.send(req)
	if err != nil {
		return nil, ocispec.Descriptor{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ocispec.Descriptor{}, statusErrorAt(at, resp)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ocispec.Descriptor{}, statusErrorAt(at, resp)
	}

	body, err := readManifest(at, resp)
	if err != nil {
		return nil, ocispec.Descriptor{}, err
	}
	if err := m.checkBound(body); err != nil {
		return nil, ocispec.Descriptor{}, err
	}

	return body, ocispec.Descriptor{
		MediaType: resp.Header.Get("Content-Type"),
		Digest:    digest.FromBytes(body),
		Size:      int64(len(body)),
	}, nil
}

// Put writes body as the manifest at the bound reference and returns the
// digest of body.
//
// The digest is the one the registry stores the manifest under, and because
// it is computed here from the same bytes that went on the wire, a caller can
// bind a signature or an index entry to it without trusting the registry to
// report it back.
//
// A repository built by [NewDigestPushRepository] has no bound tag or
// digest. Put computes the digest of body before constructing the request
// and writes to manifests/<digest>. A repository built by [NewRepository]
// still writes at the tag or digest the reference named.
func (m *Manifests) Put(ctx context.Context, mediaType string, body []byte) (digest.Digest, error) {
	endpoint := m.repo.endpoint(manifestPath(m.putTarget(body)))

	req, err := m.repo.newRequest(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mediaType)

	resp, err := m.repo.send(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", statusError(resp)
	}
	drain(resp.Body)

	return digest.FromBytes(body), nil
}

// putTarget returns the tag or digest string the write addresses. A
// digest-push repository computes the digest of body first, because that
// digest is the path; a bound repository writes at the name it was built
// with.
func (m *Manifests) putTarget(body []byte) string {
	if m.repo.manifest.publishByDigest {
		return digest.FromBytes(body).String()
	}

	return m.repo.manifest.path
}

// checkBound checks fetched manifest bytes against the digest the repository
// was built from. A repository built from a tag has nothing to check: the tag
// resolves to whatever the registry says it does.
//
// A registry that answers a digest request with different bytes has either
// corrupted them or substituted them, and both are the same problem for a
// caller whose trust in every part digest rests on this manifest.
func (m *Manifests) checkBound(body []byte) error {
	want := m.repo.manifest.digest
	if want == "" {
		return nil
	}

	// The algorithm comes from the reference, which was parsed — and so
	// validated as an available algorithm — before the repository existed.
	if got := want.Algorithm().FromBytes(body); got != want {
		return errors.New("manifest content does not match the requested digest")
	}

	return nil
}

// manifestPath returns the endpoint suffix of the manifest at target, which
// is either a tag or a digest string.
func manifestPath(target string) string {
	return "manifests/" + target
}

// readManifest reads a manifest body under maxManifestSize. It reads one byte
// past the limit so an oversized body is reported as the error it is instead
// of being silently truncated into unparseable JSON.
//
// A body that dies part way through is a connection failing, not a manifest
// being wrong, so it is marked worth another attempt — the same verdict the
// blob path reaches through its wrapped reader. A body that arrives whole and
// is simply too large is not: asking again produces the same oversized
// document.
func readManifest(at origin, resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, retry.Transient(safeCause(fmt.Sprintf("%s: read manifest response body", at), err), 0)
	}

	if len(body) > maxManifestSize {
		return nil, fmt.Errorf("%s: manifest is larger than the %d byte limit", at, maxManifestSize)
	}

	return body, nil
}
