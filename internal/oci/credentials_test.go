package oci

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestScopeFor pins the scope as a pure function of the method: reads ask
// to pull, everything else asks to pull and push.
func TestScopeFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		want   scope
	}{
		{name: "a blob check reads", method: http.MethodHead, want: "repository:team/artifact:pull"},
		{name: "a blob read reads", method: http.MethodGet, want: "repository:team/artifact:pull"},
		{name: "opening an upload writes", method: http.MethodPost, want: "repository:team/artifact:pull,push"},
		{name: "completing an upload writes", method: http.MethodPut, want: "repository:team/artifact:pull,push"},
		{
			name:   "anything else is assumed to write",
			method: http.MethodPatch,
			want:   "repository:team/artifact:pull,push",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, scopeFor(tt.method, "team/artifact"))
		})
	}
}

// TestMergeScopes pins the union a token request asks for when a challenge
// carries its own scope beside what the method needs.
func TestMergeScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		want    scope
		offered []string
		merged  []scope
	}{
		{
			name:    "a challenge that asked for nothing leaves the method's scope alone",
			want:    "repository:a:pull",
			offered: nil,
			merged:  []scope{"repository:a:pull"},
		},
		{
			name:    "a challenge asking for what the method already needs adds nothing",
			want:    "repository:a:pull",
			offered: []string{"repository:a:pull"},
			merged:  []scope{"repository:a:pull"},
		},
		{
			name:    "a challenge asking for more is merged and sorted",
			want:    "repository:b:pull",
			offered: []string{"repository:a:pull,push", "repository:a:pull,push"},
			merged:  []scope{"repository:a:pull,push", "repository:b:pull"},
		},
		{
			name:    "the order the challenge listed them in does not change the result",
			want:    "repository:b:pull",
			offered: []string{"repository:c:pull", "repository:a:pull"},
			merged:  []scope{"repository:a:pull", "repository:b:pull", "repository:c:pull"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.merged, mergeScopes(tt.want, tt.offered))
		})
	}
}
