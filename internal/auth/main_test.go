package auth_test

import (
	"os"
	"path/filepath"
	"testing"
)

// The environment a credential lookup reads. Every one of these is redirected
// at a temporary directory before a single test runs, because each of them is
// a way a lookup could reach the machine's real credentials: the first three
// name the configuration file, the fourth is where a helper such as `pass`
// keeps its agent socket, and the last is where a helper program would be
// found.
const (
	// configDirEnv overrides the directory the Docker configuration lives in.
	configDirEnv = "DOCKER_CONFIG"
	// homeEnv is where the configuration is looked for on Unix when the
	// override is unset.
	homeEnv = "HOME"
	// userProfileEnv is the same thing on Windows.
	userProfileEnv = "USERPROFILE"
	// runtimeDirEnv is where a credential helper's own session state lives.
	runtimeDirEnv = "XDG_RUNTIME_DIR"
	// pathEnv is where a credential helper program is found. Nothing the real
	// one is on stays on it.
	pathEnv = "PATH"
)

// The directories under the temporary root, one per role, so a test that
// asserts a path is under the right one is asserting something.
const (
	// configDirName holds the configuration file DefaultConfigPath resolves
	// to.
	configDirName = "docker"
	// homeDirName is the home directory the fallback resolves against.
	homeDirName = "home"
	// runtimeDirName is the runtime directory a helper would use.
	runtimeDirName = "run"
	// helperDirName is the only directory on the test PATH, and the only
	// place a credential helper the tests write can be found.
	helperDirName = "bin"
)

// tempDirPerm is the mode the temporary directories are created with. They
// stand in for a home directory, which is the owner's business and nobody
// else's.
const tempDirPerm os.FileMode = 0o700

// TestMain points every environment variable a credential lookup reads at a
// temporary directory of its own, and only then runs the tests.
//
// Isolation is not a convenience here, it is the thing under test. A
// credential lookup's whole job is to find the developer's real credentials,
// so a suite that leaves the environment alone would be running the code
// against a live keychain: the positive rows could pass on a machine somebody
// had run `docker login` on and fail on a machine nobody had, the negative
// rows could pass because a helper was missing rather than because it was
// never asked for, and any bug that wrote to the configuration would write to
// the real one. With the environment redirected, every row starts from
// nothing, and the paths the tests assert on prove which file was read.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "bigoci-auth")
	if err != nil {
		panic("create the temporary root the credential tests run under: " + err.Error())
	}

	if err := isolate(root); err != nil {
		_ = os.RemoveAll(root)

		panic("isolate the credential tests from the real environment: " + err.Error())
	}

	code := m.Run()

	_ = os.RemoveAll(root)

	os.Exit(code)
}

// isolate creates the temporary directories the tests run against and points
// the environment at them.
func isolate(root string) error {
	dirs := map[string]string{
		configDirEnv:   configDirName,
		homeEnv:        homeDirName,
		userProfileEnv: homeDirName,
		runtimeDirEnv:  runtimeDirName,
		pathEnv:        helperDirName,
	}

	for variable, name := range dirs {
		dir := filepath.Join(root, name)

		if err := os.MkdirAll(dir, tempDirPerm); err != nil {
			return err
		}

		if err := os.Setenv(variable, dir); err != nil {
			return err
		}
	}

	return nil
}
