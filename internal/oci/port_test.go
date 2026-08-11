package oci_test

import (
	"github.com/imgoci/bigoci/internal/oci"
	"github.com/imgoci/bigoci/internal/transfer"
)

// The adapter satisfies the ports the core depends on. Nothing in the oci
// package imports the transfer package — an adapter that named its port would
// point the dependency the wrong way — so these assertions are where the two
// are held together, and they fail at compile time when a signature drifts.
var (
	_ transfer.Blobs     = (*oci.Blobs)(nil)
	_ transfer.Manifests = (*oci.Manifests)(nil)
)
