package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// newCohortID fingerprints the effective spec and the harness that will run
// it. Resume may only append to rows carrying the same fingerprint.
func newCohortID(spec *Spec, commit string) (string, error) {
	harness, err := harnessIdentity(commit)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{
		"harness":    harness,
		"row_schema": rowSchema,
		"spec":       spec,
	})
	if err != nil {
		return "", fmt.Errorf("encode cohort identity: %w", err)
	}

	digest := sha256.Sum256(payload)

	return hex.EncodeToString(digest[:]), nil
}

// harnessIdentity returns a stable clean revision when one is available. A
// dirty or revision-less build uses its executable digest so changed code
// cannot claim the same resume cohort merely because both builds say "+".
func harnessIdentity(commit string) (string, error) {
	if commit != "" && !strings.HasSuffix(commit, "+") {
		return "commit:" + commit, nil
	}

	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate harness executable: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open harness executable: %w", err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash harness executable: %w", err)
	}

	return "binary:" + hex.EncodeToString(hash.Sum(nil)), nil
}
