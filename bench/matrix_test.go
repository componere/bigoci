package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// matrixSpec returns a parsed two-target spec whose cross-product the
// tests can count by hand.
func matrixSpec(t *testing.T) *Spec {
	t.Helper()

	spec := &Spec{
		RunID: "unit-run",
		Targets: []Target{
			{Name: "zot", Endpoint: "e", RepoPrefix: "bench"},
			{Name: "dist", Endpoint: "f", RepoPrefix: "bench"},
		},
		Scenarios:  []string{scenarioColdPush},
		PartSizes:  []string{"4MiB", "8MiB"},
		Workers:    []int{1, 4},
		FileSizes:  []string{"16MiB"},
		Iterations: 3,
	}
	require.NoError(t, spec.validate())

	return spec
}

func TestExpandWalksTheFullCrossProduct(t *testing.T) {
	t.Parallel()

	cells := expand(matrixSpec(t))

	require.Len(t, cells, 8, "2 targets x 2 part sizes x 2 worker counts x 1 file size")

	assert.Equal(t, "zot-p4MiB-w1-f16MiB", cells[0].id, "first cell follows spec order")
	assert.Equal(t, "zot-p4MiB-w4-f16MiB", cells[1].id, "workers vary before part sizes")
	assert.Equal(t, "dist-p4MiB-w1-f16MiB", cells[4].id, "second target follows the first completely")

	seen := map[string]bool{}
	for _, c := range cells {
		assert.False(t, seen[c.id], "cell ID %s must be unique", c.id)
		seen[c.id] = true
	}
}

func TestCellRepositoryIsolatesRunsAndCells(t *testing.T) {
	t.Parallel()

	cells := expand(matrixSpec(t))

	assert.Equal(t, "bench/unit-run/zot-p4mib-w1-f16mib", cells[0].repository("unit-run"),
		"a repository name must be entirely lowercase, whatever the cell ID reads")
	assert.NotEqual(t, cells[0].repository("run-a"), cells[0].repository("run-b"),
		"two runs must never share a repository")
}

func TestCellPartsIsACeilingDivision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileSize int64
		partSize int64
		want     int64
	}{
		{name: "exact split", fileSize: 16 << 20, partSize: 4 << 20, want: 4},
		{name: "remainder adds a part", fileSize: (16 << 20) + 1, partSize: 4 << 20, want: 5},
		{name: "file smaller than part is one part", fileSize: 1 << 20, partSize: 4 << 20, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := cell{partSize: tt.partSize, fileSize: tt.fileSize}
			assert.Equal(t, tt.want, c.parts())
		})
	}
}
