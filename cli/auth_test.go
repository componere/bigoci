package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The credential the gate in this file accepts, and the file the CLI is
// expected to find it in.
const (
	// gateUser is the account name the gate knows.
	gateUser = "ci"
	// gatePassword is that account's secret.
	gatePassword = "s3cret"
	// gateWrongPassword is a secret the gate does not know.
	gateWrongPassword = "not-the-password"
	// dockerConfigEnv points a credential lookup at the directory holding the
	// Docker configuration file. [TestMain] has already pointed it at an empty
	// one, so a row that plants a credential is the only thing that can make
	// one visible.
	dockerConfigEnv = "DOCKER_CONFIG"
	// dockerConfigName is the configuration file inside that directory.
	dockerConfigName = "config.json"
	// dockerConfigMode is the mode it is written with.
	dockerConfigMode = 0o600
)

// TestARegistryThatDemandsACredentialNobodyHasExitsSix is the negative
// control for this file, and the exit-code contract for a refused transfer.
// It runs first because everything below it is vacuous without it: a gate
// that would serve an unauthenticated push proves nothing about credentials
// reaching the wire.
//
// Both rows leave the environment pointing at a directory with no
// configuration in it, which is how a caller proves from the outside that a
// run carried no credential: it is the same thing as running with
// DOCKER_CONFIG set to an empty directory on the command line.
func TestARegistryThatDemandsACredentialNobodyHasExitsSix(t *testing.T) {
	reg := newFakeRegistry(t)
	gate := newBasicGate(t, reg)
	src, _ := fixture(t)
	ref := gate + "/" + fakeRepo + ":" + fakeTag

	t.Run("no configuration at all", func(t *testing.T) {
		t.Setenv(dockerConfigEnv, t.TempDir())

		assertExitsUnauthorized(t, runCLI(t, cmdPush, "-plain-http", "-part-size", fixturePartSize, src, ref))
	})

	t.Run("a configuration holding the wrong password", func(t *testing.T) {
		useDockerConfig(t, gate, gateWrongPassword)

		assertExitsUnauthorized(t, runCLI(t, cmdPush, "-plain-http", "-part-size", fixturePartSize, src, ref))
	})

	assert.Zero(t, reg.blobCount(), "a refused push stores nothing")
}

// TestCredentialsFromTheDockerConfigReachTheRegistry is the gate on the one
// thing this CLI does that no flag can switch off.
//
// Every run asks the library for the credentials `docker login` stores, and
// the only honest proof of that is a registry that refuses a transfer
// without one — the control above — and serves it with one, which is this
// row: the same command line against the same registry, differing only in
// the file the environment points at.
func TestCredentialsFromTheDockerConfigReachTheRegistry(t *testing.T) {
	reg := newFakeRegistry(t)
	gate := newBasicGate(t, reg)
	src, _ := fixture(t)

	useDockerConfig(t, gate, gatePassword)

	got := runCLI(t, cmdPush, "-plain-http", "-part-size", fixturePartSize, src, gate+"/"+fakeRepo+":"+fakeTag)

	require.Equal(t, exitOK, got.code, got.stderr)
	assert.True(t, isDigest(strings.TrimSuffix(got.stdout, "\n")), "a push that authenticated writes its digest")
	assert.Equal(t, fixtureBlobs, reg.blobCount(), "every part and the config blob must have landed")
}

// newBasicGate returns a registry address in front of reg that answers every
// request without the fixture credential with a challenge, and forwards the
// rest.
//
// Basic is the scheme on purpose: what this file tests is that the CLI hands
// the library a credential source at all, and Basic is the shortest path from
// a configuration file to a header on the wire. The token exchange has gates
// of its own in the library's end-to-end suite.
func newBasicGate(t *testing.T, reg *fakeRegistry) string {
	t.Helper()

	origin, err := url.Parse(reg.server.URL)
	require.NoError(t, err)

	proxy := httputil.NewSingleHostReverseProxy(origin)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if user, password, ok := req.BasicAuth(); !ok || user != gateUser || password != gatePassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="the gate"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)

			return
		}

		proxy.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)

	return strings.TrimPrefix(server.URL, "http://")
}

// useDockerConfig writes the Docker configuration a `docker login` to host as
// the gate's one account would have written, and points the environment at it
// for the length of the test. The account name never varies; the secret beside
// it is what a row chooses.
func useDockerConfig(t *testing.T, host, password string) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			host: map[string]string{
				"auth": base64.StdEncoding.EncodeToString([]byte(gateUser + ":" + password)),
			},
		},
	})
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, dockerConfigName), body, dockerConfigMode))
	t.Setenv(dockerConfigEnv, dir)
}

// assertExitsUnauthorized checks the whole refused-transfer contract: the exit
// code, the empty standard output, and the second failure line naming the
// sentinel a shell script watches for.
func assertExitsUnauthorized(t *testing.T, got result) {
	t.Helper()

	assert.Equal(t, exitUnauthorized, got.code, got.stderr)
	assert.Empty(t, got.stdout, "a transfer the registry refused writes no digest")
	assert.Contains(t, got.stderr, "bigoci: matched sentinel bigoci.ErrUnauthorized (exit 6)\n")
}
