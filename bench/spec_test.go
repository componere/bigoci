package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validSpecJSON is a spec every field of which is exercised somewhere in
// the suite. Tests derive broken variants from it rather than building
// specs by hand.
const validSpecJSON = `{
  "run_id": "unit-run",
  "targets": [
    {"name": "zot", "endpoint": "REGISTRY:5000", "plain_http": true, "repo_prefix": "bench"},
    {"name": "ghcr", "endpoint": "ghcr.io", "repo_prefix": "owner/bench", "auth_env": "GHCR"}
  ],
  "scenarios": ["cold-push", "warm-push", "cold-pull"],
  "part_sizes": ["4MiB", "8MiB"],
  "workers": [1, 4],
  "file_sizes": ["16MiB"],
  "iterations": 2,
  "verify": "first"
}`

// writeSpec drops content into a temp file and returns its path.
func writeSpec(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "spec.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func TestLoadSpecParsesTheFullShape(t *testing.T) {
	t.Parallel()

	spec, err := loadSpec(writeSpec(t, validSpecJSON))
	require.NoError(t, err)

	assert.Equal(t, "unit-run", spec.RunID)
	assert.Len(t, spec.Targets, 2)
	assert.Equal(t, []int64{4 << 20, 8 << 20}, spec.partSizes, "part sizes parse in file order")
	assert.Equal(t, []int64{16 << 20}, spec.fileSizes)
	assert.True(t, spec.Targets[0].PlainHTTP)
	assert.Equal(t, "GHCR", spec.Targets[1].AuthEnv)
}

func TestLoadSpecRejectsBrokenSpecs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		wantIn  string
	}{
		{
			name:    "unknown field",
			content: `{"run_id": "x", "unknown": true}`,
			wantIn:  "unknown",
		},
		{
			name:    "uppercase run id",
			content: `{"run_id": "Stage1"}`,
			wantIn:  "run_id",
		},
		{
			name: "unknown scenario",
			content: `{"run_id": "x", "targets": [{"name": "zot", "endpoint": "e", "repo_prefix": "b"}],
			  "scenarios": ["hot-push"], "part_sizes": ["1MiB"], "workers": [1], "file_sizes": ["1MiB"],
			  "iterations": 1}`,
			wantIn: "hot-push",
		},
		{
			name: "duplicate scenario",
			content: `{"run_id": "x", "targets": [{"name": "zot", "endpoint": "e", "repo_prefix": "b"}],
			  "scenarios": ["cold-push", "cold-push"], "part_sizes": ["1MiB"], "workers": [1],
			  "file_sizes": ["1MiB"], "iterations": 1}`,
			wantIn: "appears twice",
		},
		{
			name: "duplicate target name",
			content: `{"run_id": "x", "targets": [
			    {"name": "zot", "endpoint": "e", "repo_prefix": "b"},
			    {"name": "zot", "endpoint": "f", "repo_prefix": "c"}],
			  "scenarios": ["cold-push"], "part_sizes": ["1MiB"], "workers": [1], "file_sizes": ["1MiB"],
			  "iterations": 1}`,
			wantIn: "twice",
		},
		{
			name: "uppercase repo prefix",
			content: `{"run_id": "x", "targets": [{"name": "zot", "endpoint": "e", "repo_prefix": "Bench"}],
			  "scenarios": ["cold-push"], "part_sizes": ["1MiB"], "workers": [1], "file_sizes": ["1MiB"],
			  "iterations": 1}`,
			wantIn: "repo_prefix",
		},
		{
			name: "zero workers",
			content: `{"run_id": "x", "targets": [{"name": "zot", "endpoint": "e", "repo_prefix": "b"}],
			  "scenarios": ["cold-push"], "part_sizes": ["1MiB"], "workers": [0], "file_sizes": ["1MiB"],
			  "iterations": 1}`,
			wantIn: "workers",
		},
		{
			name: "duplicate workers",
			content: `{"run_id": "x", "targets": [{"name": "zot", "endpoint": "e", "repo_prefix": "b"}],
			  "scenarios": ["cold-push"], "part_sizes": ["1MiB"], "workers": [2, 2],
			  "file_sizes": ["1MiB"], "iterations": 1}`,
			wantIn: "appears twice",
		},
		{
			name: "zero iterations",
			content: `{"run_id": "x", "targets": [{"name": "zot", "endpoint": "e", "repo_prefix": "b"}],
			  "scenarios": ["cold-push"], "part_sizes": ["1MiB"], "workers": [1], "file_sizes": ["1MiB"],
			  "iterations": 0}`,
			wantIn: "iterations",
		},
		{
			name: "decimal SI part size",
			content: `{"run_id": "x", "targets": [{"name": "zot", "endpoint": "e", "repo_prefix": "b"}],
			  "scenarios": ["cold-push"], "part_sizes": ["500MB"], "workers": [1], "file_sizes": ["1MiB"],
			  "iterations": 1}`,
			wantIn: "decimal SI",
		},
		{
			name: "duplicate part size across spellings",
			content: `{"run_id": "x", "targets": [{"name": "zot", "endpoint": "e", "repo_prefix": "b"}],
			  "scenarios": ["cold-push"], "part_sizes": ["1MiB", "1024KiB"], "workers": [1],
			  "file_sizes": ["1MiB"], "iterations": 1}`,
			wantIn: "twice",
		},
		{
			name: "unknown verify policy",
			content: `{"run_id": "x", "targets": [{"name": "zot", "endpoint": "e", "repo_prefix": "b"}],
			  "scenarios": ["cold-push"], "part_sizes": ["1MiB"], "workers": [1], "file_sizes": ["1MiB"],
			  "iterations": 1, "verify": "sometimes"}`,
			wantIn: "verify",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadSpec(writeSpec(t, tt.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantIn)
		})
	}
}

// TestStage4GHCRDoesNotCapConfiguredWorkers guards the real-registry matrix
// against labeling a capped cell as an eight-worker measurement again.
func TestStage4GHCRDoesNotCapConfiguredWorkers(t *testing.T) {
	t.Parallel()

	spec, err := loadSpec("specs/stage4-ghcr.json")
	require.NoError(t, err)

	for _, c := range expand(spec) {
		assert.Equal(t, c.workers, maxActiveWorkers(c.workers, c.parts()), c.id)
	}
}

func TestParseSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text    string
		want    int64
		wantErr bool
	}{
		{text: "512MiB", want: 512 << 20},
		{text: "1GiB", want: 1 << 30},
		{text: "64m", want: 64 << 20},
		{text: "1024", want: 1024},
		{text: "16KiB", want: 16 << 10},
		{text: "", wantErr: true},
		{text: "1.5GiB", wantErr: true},
		{text: "512 MiB", wantErr: true},
		{text: "-1MiB", wantErr: true},
		{text: "0", wantErr: true},
		{text: "1TiB", wantErr: true},
		{text: "100MB", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			t.Parallel()

			got, err := parseSize(tt.text)
			if tt.wantErr {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatSizeRoundTrips(t *testing.T) {
	t.Parallel()

	for _, size := range []int64{1, 1023, 16 << 10, 64 << 20, 512 << 20, 1 << 30, (1 << 30) + 512} {
		text := formatSize(size)
		parsed, err := parseSize(text)
		require.NoError(t, err, "formatSize(%d) = %q must parse", size, text)
		assert.Equal(t, size, parsed, "round trip of %d through %q", size, text)
	}
}

func TestVerifyIterationPolicies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		policy string
		iter   int
		want   bool
	}{
		{policy: "none", iter: 0, want: false},
		{policy: "first", iter: 0, want: true},
		{policy: "first", iter: 1, want: false},
		{policy: "", iter: 0, want: true},
		{policy: "", iter: 2, want: false},
		{policy: "all", iter: 5, want: true},
	}

	for _, tt := range tests {
		spec := &Spec{Verify: tt.policy}
		assert.Equal(t, tt.want, spec.verifyIteration(tt.iter), "policy %q iteration %d", tt.policy, tt.iter)
	}
}
