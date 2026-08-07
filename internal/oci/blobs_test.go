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

	"github.com/componere/bigoci/internal/oci"
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
		name         string
		offset       int64
		status       int
		send         string
		contentRange string
		wantRange    string
		wantBody     string
		wantErrIs    error
		wantErr      bool
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
			wantBody:     blobPayload[resumeOffset:],
		},
		{
			name:      "a registry that ignores the range is an error",
			offset:    resumeOffset,
			status:    http.StatusOK,
			send:      blobPayload,
			wantRange: "bytes=5-",
			wantErr:   true,
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
			name:      "a blob the registry does not hold is not found",
			offset:    0,
			status:    http.StatusNotFound,
			wantErrIs: oci.ErrNotFound,
		},
		{
			name:    "a server failure is an error",
			offset:  0,
			status:  http.StatusInternalServerError,
			wantErr: true,
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
			body, err := repo.Blobs().Get(t.Context(), dgst, tt.offset)

			request := rec.only(t)
			assert.Equal(t, http.MethodGet, request.method)
			assert.Equal(t, "/v2/"+repoName+"/blobs/"+dgst.String(), request.path)
			assert.Equal(t, tt.wantRange, request.header.Get("Range"))

			if tt.wantErr || tt.wantErrIs != nil {
				require.Error(t, err)
				if tt.wantErrIs != nil {
					require.ErrorIs(t, err, tt.wantErrIs)
				}

				return
			}

			require.NoError(t, err)
			defer body.Close()

			got, err := io.ReadAll(body)
			require.NoError(t, err)
			assert.Equal(t, tt.wantBody, string(got))
		})
	}
}

func TestBlobsGetRejectsNegativeOffset(t *testing.T) {
	t.Parallel()

	var rec recorder
	repo := newRegistry(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
	}), repoName+":"+tag)

	_, err := repo.Blobs().Get(t.Context(), digest.FromString(blobPayload), -1)

	require.Error(t, err)
	assert.Empty(t, rec.all(), "a bad offset must not reach the registry")
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

			require.NoError(t, repo.Blobs().Put(t.Context(), dgst, size, body))

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
			wantRequests: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rec recorder
			repo := newRegistry(t, uploadHandler(t, &rec, tt.responses), repoName+":"+tag)

			err := repo.Blobs().Put(
				t.Context(),
				digest.FromString(blobPayload),
				int64(len(blobPayload)),
				opaqueReader{r: strings.NewReader(blobPayload)},
			)

			require.Error(t, err)
			assert.Len(t, rec.all(), tt.wantRequests)
		})
	}
}

func TestBlobsPutRejectsNegativeSize(t *testing.T) {
	t.Parallel()

	var rec recorder
	repo := newRegistry(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		rec.record(t, r)
	}), repoName+":"+tag)

	err := repo.Blobs().Put(t.Context(), digest.FromString(blobPayload), -1, strings.NewReader(blobPayload))

	require.Error(t, err)
	assert.Empty(t, rec.all(), "a bad size must not reach the registry")
}

// uploadResponses is how a fake registry answers the two requests a blob
// upload makes.
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
// responses and records both requests.
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
