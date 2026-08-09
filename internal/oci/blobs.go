package oci

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	digest "github.com/opencontainers/go-digest"
)

// mediaTypeBlob is the content type a blob upload declares. A blob is opaque
// bytes to the registry, and the distribution spec fixes the value.
const mediaTypeBlob = "application/octet-stream"

// uploadsPath is the endpoint suffix that opens an upload session. The
// trailing slash is part of the path the spec defines, and registries answer
// a request without it with 404.
const uploadsPath = "blobs/uploads/"

// digestParam is the query parameter that names the blob an upload session
// completes into.
const digestParam = "digest"

// Blobs reads and writes the blobs of one repository. It implements the
// transfer package's Blobs port and comes from [Repository.Blobs].
//
// Blobs holds no state of its own, so a transfer may run every part of a file
// through one value concurrently.
type Blobs struct {
	// repo is the repository whose blob endpoints this adapter talks to.
	repo *Repository
}

// Exists reports whether the repository holds the blob dgst names.
//
// A 404 is the registry answering the question, so a blob it does not hold is
// (false, nil) rather than an error. This is the check a push makes for every
// part before uploading it, and on a first push the answer is "no" every
// time. Any other unexpected status is an error naming the method, the path,
// and the status.
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
		return false, statusError(resp)
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
// A blob the registry does not hold is an error wrapping [ErrNotFound].
func (b *Blobs) Get(ctx context.Context, dgst digest.Digest, off int64) (io.ReadCloser, int64, error) {
	req, err := b.repo.newRequest(ctx, http.MethodGet, b.repo.endpoint(blobPath(dgst)), nil)
	if err != nil {
		return nil, 0, err
	}
	if off > 0 {
		req.Header.Set("Range", rangeFrom(off))
	}

	resp, err := b.repo.send(req)
	if err != nil {
		return nil, 0, err
	}

	start, err := blobReadStart(resp, off)
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
// Nothing buffers: the PUT reads r straight onto the wire, so a caller may
// hand Put a hundreds-of-megabytes section of a file. Put declares size as
// the request's Content-Length, because [net/http] measures only bodies it
// recognizes ([bytes.Buffer], [bytes.Reader], [strings.Reader]) and falls
// back to chunked transfer encoding for everything else — including the
// [io.SectionReader] a push streams a part from — which some registries
// reject.
func (b *Blobs) Put(ctx context.Context, dgst digest.Digest, size int64, r io.Reader) error {
	session, err := b.openUpload(ctx)
	if err != nil {
		return err
	}

	return b.completeUpload(ctx, session, dgst, size, r)
}

// openUpload opens an upload session and returns the URL the content belongs
// at. The spec lets the registry answer with a relative Location, so the
// header is resolved against the URL the request ended up at rather than
// used as it arrived.
func (b *Blobs) openUpload(ctx context.Context) (*url.URL, error) {
	endpoint := b.repo.endpoint(uploadsPath)

	req, err := b.repo.newRequest(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := b.repo.send(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return nil, statusError(resp)
	}
	drain(resp.Body)

	location := resp.Header.Get("Location")
	if location == "" {
		return nil, fmt.Errorf("POST %s: registry opened an upload but sent no Location header", endpoint.Path)
	}

	session, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("POST %s: registry sent an unusable Location %q: %w", endpoint.Path, location, err)
	}

	return resp.Request.URL.ResolveReference(session), nil
}

// completeUpload streams size bytes of r into the session and names the
// digest the registry should store the result under.
func (b *Blobs) completeUpload(
	ctx context.Context,
	session *url.URL,
	dgst digest.Digest,
	size int64,
	r io.Reader,
) error {
	req, err := b.repo.newRequest(ctx, http.MethodPut, withDigest(session, dgst), uploadBody(size, r))
	if err != nil {
		return err
	}

	req.ContentLength = size
	req.Header.Set("Content-Type", mediaTypeBlob)

	resp, err := b.repo.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return statusError(resp)
	}
	drain(resp.Body)

	return nil
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

// uploadBody returns the request body for an upload of size bytes read from
// r.
//
// Wrapping r hides its concrete type from [net/http.NewRequestWithContext],
// so every upload takes the same path through the transport: an unknown
// length that [Blobs.Put] then declares. A zero-length upload is the one case
// that declaration cannot cover — [net/http] reads a non-nil body with a zero
// length as "length unknown" and turns it chunked — so it sends no body at
// all.
func uploadBody(size int64, r io.Reader) io.ReadCloser {
	if size == 0 {
		return http.NoBody
	}

	return io.NopCloser(r)
}

// withDigest returns session with the digest query parameter that completes
// an upload. The registry may have put its own parameters in the session URL,
// so the query is parsed and re-encoded instead of concatenated onto the end.
func withDigest(session *url.URL, dgst digest.Digest) *url.URL {
	complete := *session

	query := complete.Query()
	query.Set(digestParam, dgst.String())
	complete.RawQuery = query.Encode()

	return &complete
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
// glance; either arm below would route it to the same [statusError], whose
// status is what [ErrNotFound] matches on.
func blobReadStart(resp *http.Response, off int64) (int64, error) {
	if resp.StatusCode == http.StatusNotFound {
		return 0, statusError(resp)
	}

	if off == 0 {
		if resp.StatusCode != http.StatusOK {
			return 0, statusError(resp)
		}

		return 0, nil
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if err := checkRangeStart(resp, off); err != nil {
			return 0, err
		}

		return off, nil
	case http.StatusOK:
		return 0, nil
	default:
		return 0, statusError(resp)
	}
}

// checkRangeStart confirms a 206 response starts at the byte the request asked
// for. A 206 whose Content-Range starts elsewhere — or that carries none —
// would hand the caller a stream at the wrong position and silently corrupt
// the file being assembled, so it is an error rather than something to seek
// around.
func checkRangeStart(resp *http.Response, offset int64) error {
	contentRange := resp.Header.Get("Content-Range")

	first, ok := rangeStart(contentRange)
	if !ok {
		return fmt.Errorf(
			"%s %s: registry answered the range request with an unusable Content-Range %q",
			resp.Request.Method, resp.Request.URL.Path, contentRange,
		)
	}
	if first != offset {
		return fmt.Errorf(
			"%s %s: asked for bytes from %d but the registry's range starts at %d",
			resp.Request.Method, resp.Request.URL.Path, offset, first,
		)
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
