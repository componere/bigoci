package oci_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/oci"
	"github.com/imgoci/bigoci/internal/retry"
)

// blobPayload is the content the blob fixtures move. The bytes are
// arbitrary; the length is not, because the tests assert the declared
// Content-Length against it.
const blobPayload = "bigoci blob payload"

// resumeOffset is the offset the range-request fixtures resume from. It sits
// inside blobPayload so the remainder is a proper, non-empty suffix.
const resumeOffset = 5

// sessionPath is the upload session path the fake registries hand back in
// their Location header.
const sessionPath = "/v2/" + repoName + "/blobs/uploads/session-1"

// sessionQuery is a query parameter the fake registries hang off the session
// URL, standing in for the session state real registries put there. It must
// survive the digest parameter being added.
const sessionQuery = "state=xyz"

// opaqueReader hides the concrete type of the bytes behind it, the way the
// [io.SectionReader] a push streams a part from does. [net/http] measures
// only the body types it recognizes, which is what makes an explicit
// Content-Length load bearing.
type opaqueReader struct {
	// r is the reader whose concrete type the wrapper hides.
	r io.Reader
}

// Read reads from the wrapped reader.
func (o opaqueReader) Read(p []byte) (int, error) {
	return o.r.Read(p)
}

func TestBlobsExists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		want    bool
		wantErr bool
	}{
		{name: "a blob the registry holds exists", status: http.StatusOK, want: true},
		{name: "a blob the registry does not hold is not an error", status: http.StatusNotFound},
		{name: "a server failure is an error", status: http.StatusInternalServerError, wantErr: true},
		{name: "a refused request is an error", status: http.StatusUnauthorized, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rec recorder
			repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(t, r)
				w.WriteHeader(tt.status)
			}), repoName+":"+tag)

			dgst := digest.FromString(blobPayload)
			got, err := repo.Blobs().Exists(t.Context(), dgst)

			request := rec.only(t)
			assert.Equal(t, http.MethodHead, request.method)
			assert.Equal(t, "/v2/"+repoName+"/blobs/"+dgst.String(), request.path)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), strconv.Itoa(tt.status))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBlobsGet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		offset        int64
		status        int
		send          string
		contentRange  string
		wantRange     string
		wantStart     int64
		wantBody      string
		wantErrIs     error
		notWant       string
		wantErr       bool
		wantTransient bool
	}{
		{
			name:     "an offset of zero reads the whole blob",
			offset:   0,
			status:   http.StatusOK,
			send:     blobPayload,
			wantBody: blobPayload,
		},
		{
			name:         "an offset reads the rest of the blob with a range request",
			offset:       resumeOffset,
			status:       http.StatusPartialContent,
			send:         blobPayload[resumeOffset:],
			contentRange: "bytes 5-18/19",
			wantRange:    "bytes=5-",
			wantStart:    resumeOffset,
			wantBody:     blobPayload[resumeOffset:],
		},
		{
			// The range was optional and the registry declined it. The body is
			// still a blob, read from its first byte, and saying so is what
			// lets the caller fall back without a second request.
			name:      "a registry that ignores the range serves the whole blob from byte zero",
			offset:    resumeOffset,
			status:    http.StatusOK,
			send:      blobPayload,
			wantRange: "bytes=5-",
			wantStart: 0,
			wantBody:  blobPayload,
		},
		{
			name:         "a range that starts at the wrong byte is an error",
			offset:       resumeOffset,
			status:       http.StatusPartialContent,
			send:         blobPayload,
			contentRange: "bytes 0-18/19",
			wantRange:    "bytes=5-",
			wantErr:      true,
		},
		{
			name:      "a partial response without a content range is an error",
			offset:    resumeOffset,
			status:    http.StatusPartialContent,
			send:      blobPayload[resumeOffset:],
			wantRange: "bytes=5-",
			wantErr:   true,
		},
		{
			name:         "a peer-selected content range is not reflected",
			offset:       resumeOffset,
			status:       http.StatusPartialContent,
			send:         blobPayload[resumeOffset:],
			contentRange: "reusable-content-range-bearer-a8f4c2",
			wantRange:    "bytes=5-",
			notWant:      "reusable-content-range-bearer-a8f4c2",
			wantErr:      true,
		},
		{
			name:          "a range the registry refuses outright is a terminal error",
			offset:        resumeOffset,
			status:        http.StatusRequestedRangeNotSatisfiable,
			wantRange:     "bytes=5-",
			wantErr:       true,
			wantTransient: false,
		},
		{
			name:      "a blob the registry does not hold is not found",
			offset:    0,
			status:    http.StatusNotFound,
			wantErrIs: oci.ErrNotFound,
		},
		{
			name:      "a ranged read of a blob the registry does not hold is not found",
			offset:    resumeOffset,
			status:    http.StatusNotFound,
			wantRange: "bytes=5-",
			wantErrIs: oci.ErrNotFound,
		},
		{
			name:          "a server failure is an error",
			offset:        0,
			status:        http.StatusInternalServerError,
			wantErr:       true,
			wantTransient: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rec recorder
			repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(t, r)
				if tt.contentRange != "" {
					w.Header().Set("Content-Range", tt.contentRange)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.send)
			}), repoName+":"+tag)

			dgst := digest.FromString(blobPayload)
			body, start, err := repo.Blobs().Get(t.Context(), dgst, tt.offset)

			request := rec.only(t)
			assert.Equal(t, http.MethodGet, request.method)
			assert.Equal(t, "/v2/"+repoName+"/blobs/"+dgst.String(), request.path)
			assert.Equal(t, tt.wantRange, request.header.Get("Range"))

			if tt.wantErr || tt.wantErrIs != nil {
				require.Error(t, err)
				if tt.notWant != "" {
					assert.NotContains(t, err.Error(), tt.notWant)
				}
				assert.Zero(t, start, "a read that failed opened no stream to report a start for")
				if tt.wantErrIs != nil {
					require.ErrorIs(t, err, tt.wantErrIs)
				}

				_, transient := retry.IsTransient(err)
				assert.Equal(t, tt.wantTransient, transient, "whether the failure is worth another attempt")

				return
			}

			require.NoError(t, err)
			defer body.Close()

			assert.Equal(t, tt.wantStart, start, "the byte the stream actually begins at")

			got, err := io.ReadAll(body)
			require.NoError(t, err)
			assert.Equal(t, tt.wantBody, string(got))
		})
	}
}

// TestBlobsGetUsesAStructuralDigestPathInErrors proves that the adapter still
// requests the manifest-selected digest while replacing it with a fixed label
// in both StatusError.Path and Error. A Bearer token can be a syntactically
// valid sha256 digest, so the wire path itself is not safe log context.
func TestBlobsGetUsesAStructuralDigestPathInErrors(t *testing.T) {
	t.Parallel()

	dgst := digest.Digest("sha256:" + strings.Repeat("a", 64))
	var rec recorder
	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		w.WriteHeader(http.StatusNotFound)
	}), repoName+":"+tag)

	_, _, err := repo.Blobs().Get(t.Context(), dgst, 0)
	require.ErrorIs(t, err, oci.ErrNotFound)

	request := rec.only(t)
	assert.Contains(t, request.path, dgst.String(), "the registry request still names the selected blob")

	var status *oci.StatusError
	require.ErrorAs(t, err, &status)
	assert.Contains(t, status.Path, "blobs/<digest>")
	assert.NotContains(t, status.Path, dgst.String())
	assert.NotContains(t, err.Error(), dgst.String())
}

func TestBlobsPut(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		payload  string
		location func(*http.Request) string
	}{
		{
			name:    "a relative location is resolved against the upload endpoint",
			payload: blobPayload,
			location: func(*http.Request) string {
				return sessionPath + "?" + sessionQuery
			},
		},
		{
			name:    "an absolute location is used as sent",
			payload: blobPayload,
			location: func(r *http.Request) string {
				return "http://" + r.Host + sessionPath + "?" + sessionQuery
			},
		},
		{
			name:    "an empty blob still declares a content length",
			payload: "",
			location: func(*http.Request) string {
				return sessionPath + "?" + sessionQuery
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rec recorder
			repo := newRegistry(t, uploadHandler(t, &rec, uploadResponses{
				location:   tt.location,
				postStatus: http.StatusAccepted,
				putStatus:  http.StatusCreated,
			}), repoName+":"+tag)

			dgst := digest.FromString(tt.payload)
			size := int64(len(tt.payload))
			body := opaqueReader{r: strings.NewReader(tt.payload)}

			require.NoError(t, repo.Blobs().Put(t.Context(), dgst, size, body, nil))

			requests := rec.all()
			require.Len(t, requests, 2)

			open := requests[0]
			assert.Equal(t, http.MethodPost, open.method)
			assert.Equal(t, "/v2/"+repoName+"/blobs/uploads/", open.path)

			complete := requests[1]
			assert.Equal(t, http.MethodPut, complete.method)
			assert.Equal(t, sessionPath, complete.path)
			assert.Equal(t, dgst.String(), complete.query.Get("digest"))
			assert.Equal(t, "xyz", complete.query.Get("state"), "the registry's own parameters must survive")
			assert.Equal(t, tt.payload, string(complete.body))
			assert.Equal(t, size, complete.contentLength)
			assert.Equal(t, strconv.FormatInt(size, 10), complete.header.Get("Content-Length"))
			assert.Empty(t, complete.transferEncoding, "an explicit length is what keeps the body unchunked")
		})
	}
}

func TestBlobsPutFailures(t *testing.T) {
	t.Parallel()

	session := func(*http.Request) string { return sessionPath + "?" + sessionQuery }
	tests := []struct {
		name         string
		responses    uploadResponses
		wantRequests int
	}{
		{
			name: "a session the registry refuses is an error",
			responses: uploadResponses{
				location:   session,
				postStatus: http.StatusInternalServerError,
				putStatus:  http.StatusCreated,
			},
			wantRequests: 1,
		},
		{
			name: "a session without a location is an error",
			responses: uploadResponses{
				location:   func(*http.Request) string { return "" },
				postStatus: http.StatusAccepted,
				putStatus:  http.StatusCreated,
			},
			wantRequests: 1,
		},
		{
			name: "a completion the registry rejects is an error",
			responses: uploadResponses{
				location:   session,
				postStatus: http.StatusAccepted,
				putStatus:  http.StatusBadRequest,
			},
			wantRequests: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rec recorder
			repo := newRegistry(t, uploadHandler(t, &rec, tt.responses), repoName+":"+tag)

			err := repo.Blobs().Put(t.Context(),
				digest.FromString(blobPayload),
				int64(len(blobPayload)),
				opaqueReader{r: strings.NewReader(blobPayload)}, nil)

			require.Error(t, err)
			assert.Len(t, rec.all(), tt.wantRequests)
		})
	}
}

func TestBlobsGetSendsAFreshRequestEveryTime(t *testing.T) {
	t.Parallel()

	var rec recorder
	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
		_, _ = io.WriteString(w, blobPayload)
	}), repoName+":"+tag)

	dgst := digest.FromString(blobPayload)

	for range 2 {
		body, _, err := repo.Blobs().Get(t.Context(), dgst, 0)
		require.NoError(t, err)

		got, err := io.ReadAll(body)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		assert.Equal(t, blobPayload, string(got))
	}

	assert.Len(t, rec.all(), 2, "a read carries no state from the one before it, so a retry re-resolves everything")
}

func TestBlobsGetTagsABodyThatBreaksMidRead(t *testing.T) {
	t.Parallel()

	// The registry promises a body far longer than it sends, so the read
	// fails part way through rather than ending early and cleanly.
	const declared = 1 << 16

	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(declared))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.Repeat("a", declared/2))
		resetConnection(t, w)
	}), repoName+":"+tag)

	body, _, err := repo.Blobs().Get(t.Context(), digest.FromString(blobPayload), 0)
	require.NoError(t, err, "the request succeeded; only the body dies")

	defer body.Close()

	_, err = io.ReadAll(body)

	require.Error(t, err)

	_, transient := retry.IsTransient(err)
	assert.True(t, transient, "a connection that broke mid-part is worth another attempt")
}

func TestBlobsPutTagsAConnectionResetMidUpload(t *testing.T) {
	t.Parallel()

	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Location", sessionPath+"?"+sessionQuery)
			w.WriteHeader(http.StatusAccepted)

			return
		}

		dropConnection(t, w)
	}), repoName+":"+tag)

	err := repo.Blobs().Put(t.Context(),
		digest.FromString(blobPayload),
		int64(len(blobPayload)),
		opaqueReader{r: strings.NewReader(blobPayload)}, nil)

	require.Error(t, err)

	_, transient := retry.IsTransient(err)
	assert.True(t, transient, "an upload the connection cut short is worth another attempt")
}

func TestBlobsPutLeavesRetryingToItsCaller(t *testing.T) {
	t.Parallel()

	var rec recorder
	repo := newRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)

		if r.Method == http.MethodPost {
			w.Header().Set("Location", sessionPath+"?"+sessionQuery)
			w.WriteHeader(http.StatusAccepted)

			return
		}

		// The first completion fails and the second would succeed, so an
		// adapter that retried underneath the orchestrator would report
		// success here instead of a failure worth repeating.
		if uploadsSoFar(rec.all()) == 1 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		w.WriteHeader(http.StatusCreated)
	}), repoName+":"+tag)

	err := repo.Blobs().Put(t.Context(),
		digest.FromString(blobPayload),
		int64(len(blobPayload)),
		opaqueReader{r: strings.NewReader(blobPayload)}, nil)

	require.Error(t, err)

	_, transient := retry.IsTransient(err)
	assert.True(t, transient, "the verdict is all the adapter owes")
	assert.Equal(t, 1, uploadsSoFar(rec.all()), "the attempt budget belongs to the orchestrator alone")
}

// uploadsSoFar counts the completing PUTs a fake registry has seen, which is
// how many upload attempts the adapter made.
func uploadsSoFar(requests []recorded) int {
	uploads := 0

	for _, request := range requests {
		if request.method == http.MethodPut {
			uploads++
		}
	}

	return uploads
}

// uploadResponses is how a fake registry answers the POST and PUT a blob
// upload makes. A failed PUT may be followed by best-effort DELETE cleanup.
type uploadResponses struct {
	// location builds the Location header the session request answers with,
	// from the request that opened it. An empty string sends no header.
	location func(*http.Request) string
	// postStatus is the status the session request is answered with.
	postStatus int
	// putStatus is the status the completing upload is answered with.
	putStatus int
}

// uploadHandler returns a fake registry that answers a blob upload with
// responses and records every request.
func uploadHandler(t *testing.T, rec *recorder, responses uploadResponses) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(t, r)

		switch r.Method {
		case http.MethodPost:
			if location := responses.location(r); location != "" {
				w.Header().Set("Location", location)
			}
			w.WriteHeader(responses.postStatus)
		case http.MethodPut:
			w.WriteHeader(responses.putStatus)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}
