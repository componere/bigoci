package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	ociblob "github.com/imgoci/go-oci-blob"

	digest "github.com/opencontainers/go-digest"

	"github.com/imgoci/bigoci/internal/retry"
	"github.com/imgoci/bigoci/internal/transfer"
)

// uploadsPath is the endpoint suffix that opens an upload session. The
// trailing slash is part of the path the spec defines, and registries answer
// a request without it with 404.
const uploadsPath = "blobs/uploads/"

// Blobs reads and writes the blobs of one repository. It implements the
// transfer package's Blobs port and comes from [Repository.Blobs].
//
// Blobs is immutable after construction and safe to use concurrently across
// every part of one transfer.
type Blobs struct {
	// repo is the repository whose blob endpoints this adapter talks to.
	repo *Repository
	// client owns the retry-disabled upstream blob protocol implementation.
	client *ociblob.Client
	// target names the registry repository client uploads into.
	target ociblob.Repository
}

// Exists reports whether the repository holds the blob dgst names.
//
// A 404 is the registry answering the question, so a blob it does not hold is
// (false, nil) rather than an error. That reading stops at the registry's own
// origin: a registry that redirects the check to storage has already decided
// the blob exists, so a 404 from the location it named is a stale signature
// rather than an answer, and comes back as an error worth another attempt —
// matching neither [ErrNotFound] nor [ErrUnauthorized]. This is the check a
// push makes for every part before uploading it, and on a first push the
// answer is "no" every time. Any other unexpected status is an error naming
// the method, path, and status.
func (b *Blobs) Exists(ctx context.Context, dgst digest.Digest) (bool, error) {
	req, err := b.repo.newRequest(ctx, http.MethodHead, b.repo.endpoint(blobPath(dgst)), nil)
	if err != nil {
		return false, err
	}

	resp, err := b.repo.send(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, statusErrorAt(originOf(req), resp)
	}
}

// Get streams the blob dgst names from off, and reports the offset the stream
// it returns actually starts at. An off of zero reads the whole blob.
//
// The returned reader is the response body itself, unread: no blob content
// passes through a buffer on the way to the caller, who owns the reader and
// closes it. The body is wrapped only to classify its failures, so a
// connection that breaks part way through a blob is reported as worth another
// attempt even though Get itself has long since returned.
//
// A nonzero off asks for the remainder of the blob with a Range request. The
// distribution spec does not require registries to honor one, so a registry
// that answers 200 with the whole blob instead of 206 with the requested range
// is reported rather than refused: the reported offset is zero, the body is a
// blob read like any other, and what to do about starting over belongs to the
// orchestrator that asked. A 206 whose range starts at some third byte is
// still an error — that is an answer to a question nobody asked.
//
// A blob the registry does not hold is an error wrapping [ErrNotFound]. A 404
// from a storage location the registry redirected to is not that answer: it
// reads as a stale signature, comes back marked worth another attempt, and
// matches no sentinel. Public errors use a "<digest>" path label: a digest was
// selected by the manifest and can itself be a reusable Bearer token.
//
// The request asks for identity coding so the bytes the caller hashes are the
// stored bytes. A response that arrives under any other content coding is
// refused before its status or body is read, and the body is closed.
func (b *Blobs) Get(ctx context.Context, dgst digest.Digest, off int64) (io.ReadCloser, int64, error) {
	req, err := b.repo.newRequest(ctx, http.MethodGet, b.repo.endpoint(blobPath(dgst)), nil)
	if err != nil {
		return nil, 0, err
	}
	req = withOrigin(req, origin{
		method: http.MethodGet,
		path:   b.repo.endpoint("blobs/<digest>").Path,
	})
	req.Header.Set(headerAcceptEncoding, codingIdentity)
	if off > 0 {
		req.Header.Set("Range", rangeFrom(off))
	}

	resp, err := b.repo.send(req)
	if err != nil {
		return nil, 0, err
	}

	at := originOf(req)
	if err := checkIdentityEncoding(at, resp); err != nil {
		_ = resp.Body.Close()

		return nil, 0, err
	}

	start, err := blobReadStart(at, resp, off)
	if err != nil {
		_ = resp.Body.Close()

		return nil, 0, err
	}

	return &blobBody{rc: resp.Body}, start, nil
}

// Put uploads size bytes read from r as the blob dgst names.
//
// The upload is monolithic: one POST opens a session, and one PUT streams the
// content into it and names the digest. That pair is the only push primitive
// every registry implements correctly, which is why bigoci splits a file into
// parts small enough to push this way instead of reaching for chunked or
// resumable uploads.
//
// The upstream client keeps memory bounded to its transport staging buffer
// rather than buffering a part. wire receives only bytes the HTTP transport
// consumed, including failed attempts, and stops before Put returns.
func (b *Blobs) Put(
	ctx context.Context,
	dgst digest.Digest,
	size int64,
	r io.Reader,
	wire transfer.WireProgress,
) error {
	var err error
	if wire == nil {
		err = b.client.Push(ctx, b.target, dgst, size, r)
	} else {
		err = b.client.Push(ctx, b.target, dgst, size, r, ociblob.WithWireProgress(wire))
	}
	if err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return err
	}
	var targetErr *externalTargetError
	if errors.As(err, &targetErr) {
		return err
	}

	if after, ok := ociblob.Retryable(err); ok {
		return retry.Transient(err, after)
	}

	return err
}

// blobPath returns the endpoint suffix of one blob.
func blobPath(dgst digest.Digest) string {
	return "blobs/" + dgst.String()
}

// rangeFrom returns the Range header value that asks for everything from
// offset to the end of the blob.
func rangeFrom(offset int64) string {
	return "bytes=" + strconv.FormatInt(offset, 10) + "-"
}

// blobReadStart checks a blob read's response against what the request asked
// for and reports the byte the body starts at.
//
// A read from byte zero has one acceptable answer, a 200 carrying the whole
// blob. A range request has two: a 206 whose range begins where the request
// said, which starts at off, and a 200 from a registry that ignored the header
// and is sending the blob from its first byte, which starts at 0. The second
// is reported rather than refused because its body is a perfectly good blob
// read, and only the caller knows whether reading it from the beginning is
// cheaper than giving up. Every other status is a failure, 416 included: a
// registry that refuses the range outright has answered nothing usable, and
// asking again would only be refused again.
//
// The 404 is called out first only so the not-found chain is obvious at a
// glance; either arm below would route it to the same [statusErrorAt], whose
// status is what [ErrNotFound] matches on.
//
// It runs on the response that carried the content, which after a redirect is
// the object store's rather than the registry's. That changes nothing about
// the rule: a 206 from storage still has to start where the request said, and
// a 200 from storage is still the whole blob from its first byte. What it does
// change is where a failure is reported against, which is why at is threaded
// in — a storage URL has no business in an error message.
func blobReadStart(at origin, resp *http.Response, off int64) (int64, error) {
	if resp.StatusCode == http.StatusNotFound {
		return 0, statusErrorAt(at, resp)
	}

	if off == 0 {
		if resp.StatusCode != http.StatusOK {
			return 0, statusErrorAt(at, resp)
		}

		return 0, nil
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if err := checkRangeStart(at, resp, off); err != nil {
			return 0, err
		}

		return off, nil
	case http.StatusOK:
		return 0, nil
	default:
		return 0, statusErrorAt(at, resp)
	}
}

// checkRangeStart confirms a 206 response starts at the byte the request asked
// for. A 206 whose Content-Range starts elsewhere — or that carries none —
// would hand the caller a stream at the wrong position and silently corrupt
// the file being assembled, so it is an error rather than something to seek
// around.
//
// The message names the registry request at describes, but no byte values.
// The peer's raw Content-Range and parsed start are direct reflection material;
// the expected offset can in turn be derived from manifest-selected sizes.
func checkRangeStart(at origin, resp *http.Response, offset int64) error {
	contentRange := resp.Header.Get("Content-Range")

	first, ok := rangeStart(contentRange)
	if !ok {
		return fmt.Errorf("%s: the range response has an unusable Content-Range", at)
	}
	if first != offset {
		return fmt.Errorf("%s: the range response does not start at the requested offset", at)
	}

	return nil
}

// rangeStart reads the first byte position out of a Content-Range value in
// the "bytes <first>-<last>/<size>" form a 206 carries.
func rangeStart(value string) (int64, bool) {
	rest, ok := strings.CutPrefix(value, "bytes ")
	if !ok {
		return 0, false
	}

	first, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}

	n, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, false
	}

	return n, true
}
