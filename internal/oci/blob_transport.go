package oci

import (
	"context"
	"io"
	"net/http"
)

// registryBlobTransport authenticates blob-library requests against one
// repository while leaving redirect and response classification to the library.
type registryBlobTransport struct {
	// repo owns the authentication state and caller-configured HTTP client.
	repo *Repository
}

// RoundTrip authenticates and sends one registry-origin blob request.
func (t registryBlobTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request := req.Clone(req.Context())
	// Authentication never replays an upload body inside the orchestrator's
	// attempt; a refreshed credential belongs on its next fresh reader.
	if request.Body != nil && request.Body != http.NoBody {
		request.GetBody = nil
	}
	request = withOrigin(request, origin{
		method: req.Method,
		path:   t.repo.endpoint(uploadsPath).Path,
	})
	if err := t.repo.authorizeRequest(request); err != nil {
		return nil, err
	}

	send := func(outbound *http.Request) (*http.Response, error) {
		return roundTripWithClient(t.repo.client, outbound, originOf(outbound).String())
	}

	return t.repo.sendWith(request, send, t.repo.acceptedDirect)
}

// storageBlobTransport sends scrubbed off-origin requests through bigoci's
// caller-derived, destination-guarded external client.
type storageBlobTransport struct {
	// repo owns the guarded external client bound to the registry origin.
	repo *Repository
}

// RoundTrip sends one off-origin blob request without interpreting its status.
func (t storageBlobTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	operation := origin{
		method: req.Method,
		path:   t.repo.endpoint(uploadsPath).Path,
	}

	return roundTripWithClient(t.repo.external, req, operation.String())
}

// timeoutBody cancels a client timeout when the response body is closed.
type timeoutBody struct {
	// ReadCloser is the response body returned by the caller's transport.
	io.ReadCloser

	// cancel releases the timeout context and its timer.
	cancel context.CancelFunc
}

// Close closes the response body and releases its timeout context.
func (b *timeoutBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()

	return err
}

// roundTripWithClient sends req through client's transport without invoking
// client's redirect policy, which the embedding blob client owns.
func roundTripWithClient(
	client *http.Client,
	req *http.Request,
	operation string,
) (*http.Response, error) {
	request := req.Clone(req.Context())

	var cancel context.CancelFunc
	if client.Timeout > 0 {
		ctx, stop := context.WithTimeout(request.Context(), client.Timeout)
		request = request.WithContext(ctx)
		cancel = stop
	}

	if client.Jar != nil {
		for _, cookie := range client.Jar.Cookies(request.URL) {
			request.AddCookie(cookie)
		}
	}

	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	resp, err := transport.RoundTrip(request)
	if err != nil {
		if cancel != nil {
			cancel()
		}

		return nil, transportFailure(req, resp, err, operation)
	}

	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	if resp.Request == nil {
		resp.Request = request
	}
	if client.Jar != nil {
		client.Jar.SetCookies(request.URL, resp.Cookies())
	}
	if cancel != nil {
		resp.Body = &timeoutBody{ReadCloser: resp.Body, cancel: cancel}
	}

	return resp, nil
}
