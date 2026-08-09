package main

import (
	"fmt"
	"os"
	"testing"
)

// The environment variables a credential lookup reads, and what this package's
// tests replace them with.
//
// Every run of this CLI now asks the library for the Docker credentials, so
// every test in this package would otherwise read whatever configuration the
// machine running it happens to have — the maintainer's own login, or a CI
// runner's registry token. The isolation is not politeness: a test that can
// see a real credential is a test whose result depends on who ran it.
const (
	// configDirEnv points the library at the Docker configuration directory.
	// An empty temporary directory holds no config.json, which resolves every
	// registry to the anonymous credential.
	configDirEnv = "DOCKER_CONFIG"
	// homeEnv is where the library looks for .docker/config.json when
	// DOCKER_CONFIG names nothing. It is replaced too, so a test that unsets
	// DOCKER_CONFIG still cannot reach a real one.
	homeEnv = "HOME"
	// profileEnv is the same thing on Windows.
	profileEnv = "USERPROFILE"
	// pathEnv is where a credential helper would be found. It is emptied
	// rather than replaced: no test here means to run a helper, and an empty
	// PATH makes running one impossible instead of merely unlikely. These
	// tests drive the CLI in process and start no subprocess of their own, so
	// nothing else needs it.
	pathEnv = "PATH"
)

// TestMain runs this package's tests with the credential environment pointed
// at directories that hold nothing.
//
// It is the only TestMain here, and this is the whole of what it does. The
// temporary directory is removed on the way out, which is why m.Run's status
// travels through a variable rather than straight into [os.Exit]: an exit
// inside the same function would skip the cleanup.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests isolates the credential environment, runs the package's tests, and
// returns the status the process should leave with.
func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "bigoci-cli-test")
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolate the credential environment: %v\n", err)

		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	for name, value := range map[string]string{
		configDirEnv: dir,
		homeEnv:      dir,
		profileEnv:   dir,
		pathEnv:      "",
	} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintf(os.Stderr, "isolate %s: %v\n", name, err)

			return 1
		}
	}

	return m.Run()
}
