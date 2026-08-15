package oci

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci/internal/retry"
)

// reflectedCoding is a reusable credential-shaped Content-Encoding value. A
// peer controls this header, so a refusal must not repeat it.
const reflectedCoding = "reusable-encoding-bearer-a8f4c2"

func TestCheckIdentityEncoding(t *testing.T) {
	t.Parallel()

	at := origin{method: http.MethodGet, path: "/v2/team/artifact/manifests/v1"}

	tests := []struct {
		name    string
		values  []string
		wantErr bool
	}{
		{name: "an absent header is identity"},
		{name: "a blank value is identity", values: []string{""}},
		{name: "a mixed-case identity token is identity", values: []string{"Identity"}},
		{name: "repeated identity fields are identity", values: []string{"identity", "identity"}},
		{name: "comma-separated identity tokens are identity", values: []string{"identity, identity"}},
		{name: "gzip is not identity", values: []string{"gzip"}, wantErr: true},
		{name: "identity followed by gzip is not identity", values: []string{"Identity, gzip"}, wantErr: true},
		{name: "gzip in a later field is not identity", values: []string{"identity", "gzip"}, wantErr: true},
		{name: "a peer-selected coding is not reflected", values: []string{reflectedCoding}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resp := &http.Response{Header: http.Header{}}
			for _, value := range tt.values {
				resp.Header.Add(headerContentEncoding, value)
			}

			err := checkIdentityEncoding(at, resp)

			if !tt.wantErr {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)

			var coding *contentCodingError
			require.ErrorAs(t, err, &coding)
			assert.Equal(t, at, coding.at)
			assert.Equal(t, at.String()+": the response is not identity coded", err.Error())
			assert.NotContains(t, err.Error(), reflectedCoding)
			assert.NotContains(t, err.Error(), "gzip")
			assert.NotContains(t, err.Error(), "Identity")

			_, transient := retry.IsTransient(err)
			assert.False(t, transient)
		})
	}
}
