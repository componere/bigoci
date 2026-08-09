package main

import (
	"strconv"
	"strings"
)

// cell is one point of the matrix: a target registry and a transfer shape.
// Every cell owns its own repository, so no two cells can share registry
// state.
type cell struct {
	// target is the registry this cell transfers against.
	target Target
	// partSize is the part size in bytes.
	partSize int64
	// workers is the worker count.
	workers int
	// fileSize is the transfer size in bytes.
	fileSize int64
	// id is the cell's stable identity, derived from the axes. Result rows
	// and resume bookkeeping key on it.
	id string
}

// expand walks the spec's cross-product in deterministic order — targets,
// then part sizes, then workers, then file sizes, each in file order — and
// returns one cell per point.
func expand(spec *Spec) []cell {
	cells := make([]cell, 0, len(spec.Targets)*len(spec.partSizes)*len(spec.Workers)*len(spec.fileSizes))
	for _, target := range spec.Targets {
		for _, partSize := range spec.partSizes {
			for _, workers := range spec.Workers {
				for _, fileSize := range spec.fileSizes {
					cells = append(cells, cell{
						target:   target,
						partSize: partSize,
						workers:  workers,
						fileSize: fileSize,
						id:       cellID(target.Name, partSize, workers, fileSize),
					})
				}
			}
		}
	}

	return cells
}

// cellID derives a cell's identity from its axes, in the shape
// "zot-p512MiB-w4-f2GiB". The sizes render through formatSize, so an ID
// always reads the way the spec author spelled the size.
func cellID(target string, partSize int64, workers int, fileSize int64) string {
	return target + "-p" + formatSize(partSize) + "-w" + strconv.Itoa(workers) + "-f" + formatSize(fileSize)
}

// repository returns the repository path this cell transfers to under
// runID: the target's prefix, the run, and the cell, each its own path
// segment. The cell ID is lowercased on the way in — its size units read
// "MiB" for humans, but a repository name must be entirely lowercase.
func (c cell) repository(runID string) string {
	return c.target.RepoPrefix + "/" + runID + "/" + strings.ToLower(c.id)
}

// parts returns how many parts the cell's file splits into: the split rule
// is a ceiling division, matching the library's plan.
func (c cell) parts() int64 {
	return (c.fileSize + c.partSize - 1) / c.partSize
}
