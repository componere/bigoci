package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
)

// fixturePerm is the mode fixture files are created with: private to the
// operator, since the boxes the harness runs on are shared with nothing.
const fixturePerm = 0o600

// fixture is one generated source file and the digest of its content,
// computed while writing so verification never re-reads the source.
type fixture struct {
	// path is where the file was written.
	path string
	// digest is the hex sha256 of the file's content.
	digest string
}

// writeFixture generates the unique source file for one iteration of one
// cell and returns it with its content digest.
//
// The bytes are a seeded ChaCha8 stream, so generation moves at disk speed
// and an attempt can be reproduced exactly. The seed folds in the run,
// attempt, cell, and iteration identities. A new process attempt therefore
// cannot reuse blobs an interrupted push retained. Random content also makes
// a misplaced part visible; repeated bytes would hide swapped parts.
func writeFixture(dir, runID, attemptID, cellID string, iteration int, size int64) (fixture, error) {
	path := filepath.Join(dir, cellID+"-"+attemptID+"-i"+strconv.Itoa(iteration)+".src")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fixturePerm)
	if err != nil {
		return fixture{}, fmt.Errorf("create fixture: %w", err)
	}

	hasher := sha256.New()
	written, err := io.CopyN(
		io.MultiWriter(f, hasher),
		fixtureStream(runID, attemptID, cellID, iteration),
		size,
	)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fixture{}, fmt.Errorf("write %d byte fixture %s (wrote %d): %w", size, path, written, err)
	}

	return fixture{path: path, digest: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// fixtureStream returns the generator an iteration's fixture is filled
// from, seeded from the identifiers that make the attempt unique.
func fixtureStream(runID, attemptID, cellID string, iteration int) io.Reader {
	seed := sha256.Sum256([]byte(runID + "|" + attemptID + "|" + cellID + "|" + strconv.Itoa(iteration)))

	return rand.NewChaCha8(seed)
}

// hashFile returns the hex sha256 of the file at path, streamed rather than
// read into memory: pulled files are gigabytes.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open for hashing: %w", err)
	}

	hasher := sha256.New()
	_, err = io.Copy(hasher, f)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}
