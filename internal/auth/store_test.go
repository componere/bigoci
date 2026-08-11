package auth_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/credentials/trace"

	"github.com/imgoci/bigoci/internal/auth"
	"github.com/imgoci/bigoci/internal/oci"
)

// The registries the rows below are written for and looked up under.
const (
	// fixtureRegistry is an ordinary registry host carrying a port, which is
	// the shape that has to survive a lookup unrewritten.
	fixtureRegistry oci.Registry = "registry.example:5000"
	// hubRegistry is the registry a Docker Hub reference names, which is the
	// spelling users actually write.
	hubRegistry oci.Registry = "docker.io"
	// hubDialedHost is the host such a transfer really dials, the other
	// spelling a caller can write out explicitly.
	hubDialedHost oci.Registry = "registry-1.docker.io"
	// hubConfigKey is the key `docker login` files a Docker Hub credential
	// under. It is not the host above, and that mismatch is the only reason
	// the lookup translates anything at all.
	hubConfigKey = "https://index.docker.io/v1/"
)

// What the fake credential helper prints when it is asked for a credential.
const (
	// helperUser is the user name it answers with.
	helperUser = "helper-user"
	// helperSecret is the secret it answers with.
	helperSecret = "helper-secret"
)

// fakeHelperScript is a credential helper that answers every lookup with the
// same credential and records the action it was asked to perform.
//
// The record's path is written into the program rather than passed in the
// environment, because the environment is shared by the whole test binary and
// a record file must not be: a row proving a helper ran and a row proving one
// did not would otherwise be reading the same file.
const fakeHelperScript = `#!/bin/sh
echo "$1" >> %q
echo '{"ServerURL":"","Username":"` + helperUser + `","Secret":"` + helperSecret + `"}'
`

// wedgedHelperScript is a credential helper that never answers. It records
// that it started and then sleeps for longer than any test would wait.
//
// The sleep replaces the shell rather than running under it, so the one
// process the lookup knows about is the one holding its output open: a
// grandchild left behind would keep the pipe open after its parent was killed
// and the lookup would wait for the sleep it was meant to escape.
const wedgedHelperScript = `#!/bin/sh
PATH=/usr/bin:/bin
export PATH
echo "$1" >> %q
exec sleep 60
`

// wedgedHelperBudget is the longest the lookup may take before the row fails.
// It sits comfortably past the ten seconds the store allows itself and well
// short of the minute the helper above would otherwise take.
const wedgedHelperBudget = 30 * time.Second

// The modes the fixtures are written with. Both are the owner's business and
// nobody else's, which is what a file holding credentials is.
const (
	// configPerm is the mode a planted configuration file gets.
	configPerm os.FileMode = 0o600
	// helperPerm is the mode a credential helper gets: the test user also has
	// to be able to run it.
	helperPerm os.FileMode = 0o700
)

// TestStoreCredentialReadsTheConfiguration walks the shapes a Docker
// configuration stores a credential in, planted in a file only the row can
// see, and checks each comes back in the right Credential field.
func TestStoreCredentialReadsTheConfiguration(t *testing.T) {
	t.Parallel()

	planted := fmt.Sprintf(`{"auth":%q}`, basicAuth("alice", "s3cret"))

	tests := []struct {
		name     string
		config   string
		registry oci.Registry
		want     oci.Credential
	}{
		{
			name:     "an entry for the registry becomes a user name and a password",
			config:   authsConfig(string(fixtureRegistry), planted),
			registry: fixtureRegistry,
			want:     oci.Credential{Username: "alice", Password: "s3cret"},
		},
		{
			name:     "a registry the configuration does not mention is anonymous",
			config:   authsConfig(string(fixtureRegistry), planted),
			registry: "other.example",
			want:     oci.Credential{},
		},
		{
			name:     "a Docker Hub login is found for the registry a reference names",
			config:   authsConfig(hubConfigKey, fmt.Sprintf(`{"auth":%q}`, basicAuth("bob", "hub-token"))),
			registry: hubRegistry,
			want:     oci.Credential{Username: "bob", Password: "hub-token"},
		},
		{
			name:     "a Docker Hub login is found for the dialed host spelled out",
			config:   authsConfig(hubConfigKey, fmt.Sprintf(`{"auth":%q}`, basicAuth("bob", "hub-token"))),
			registry: hubDialedHost,
			want:     oci.Credential{Username: "bob", Password: "hub-token"},
		},
		{
			name:     "an identity token comes back in its own field, not as a password",
			config:   authsConfig(string(fixtureRegistry), `{"identitytoken":"refresh-me"}`),
			registry: fixtureRegistry,
			want:     oci.Credential{IdentityToken: "refresh-me"},
		},
		{
			name:     "a registry token comes back in its own field",
			config:   authsConfig(string(fixtureRegistry), `{"registrytoken":"bearer-me"}`),
			registry: fixtureRegistry,
			want:     oci.Credential{RegistryToken: "bearer-me"},
		},
		{
			name:     "a configuration holding nothing at all is anonymous",
			config:   `{}`,
			registry: fixtureRegistry,
			want:     oci.Credential{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := newStore(t, plantConfig(t, tt.config))

			got, err := store.Credential(noExec(t), tt.registry)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestStoreCredentialNeverRepeatsAMalformedEntry pins the error shape: an
// auth entry that decodes to something other than user:password is reported
// without its decoded content, because that content is usually a secret
// somebody pasted where a base64 pair belongs, and the message reaches
// terminals and CI logs verbatim.
func TestStoreCredentialNeverRepeatsAMalformedEntry(t *testing.T) {
	t.Parallel()

	const pasted = "ghp_SUPERSECRETTOKENVALUE"
	config := authsConfig(
		string(fixtureRegistry),
		fmt.Sprintf(`{"auth":%q}`, base64.StdEncoding.EncodeToString([]byte(pasted))),
	)
	path := plantConfig(t, config)
	store := newStore(t, path)

	_, err := store.Credential(noExec(t), fixtureRegistry)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), pasted, "the decoded entry is the secret and stays out of the message")
	assert.Contains(t, err.Error(), string(fixtureRegistry), "the message names the registry")
	assert.Contains(t, err.Error(), path, "and the file, which is where the fix happens")
}

// TestStoreCredentialOnAConfigurationThatIsNotThere pins the zero-config
// default: a machine nobody has logged in on resolves every registry to the
// anonymous credential, with no error anywhere on the way.
func TestStoreCredentialOnAConfigurationThatIsNotThere(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	pin(t, path)

	store, err := auth.NewStore(path)
	require.NoError(t, err, "a machine nobody has logged in on is not a failure")

	got, err := store.Credential(noExec(t), fixtureRegistry)

	require.NoError(t, err)
	assert.True(t, got.Empty(), "a configuration that is not there resolves to the anonymous credential")
}

// TestNewStoreOnAConfigurationItCannotRead pins the one failure NewStore
// owns: a file that exists but is not a configuration fails at build time,
// where the caller who asked for credentials can still be told.
func TestNewStoreOnAConfigurationItCannotRead(t *testing.T) {
	t.Parallel()

	path := plantConfig(t, `{"auths": this is not a configuration`)

	store, err := auth.NewStore(path)

	require.Error(t, err, "a caller who asked bigoci to use their credentials must not transfer anonymously instead")
	assert.Nil(t, store)
	assert.Contains(t, err.Error(), path, "the failure has to say which file could not be read")
}

// TestStoreReadsTheFileItWasGivenAndSaysWhichOneItWas is where the suite's
// isolation stops being a claim. The credential comes back from a file this
// row wrote seconds ago in a directory of its own, and the store names that
// same file when asked — so the answer cannot have come from the machine's
// real configuration, whatever is in it and whoever is logged in.
func TestStoreReadsTheFileItWasGivenAndSaysWhichOneItWas(t *testing.T) {
	t.Parallel()

	planted := authsConfig(string(fixtureRegistry), fmt.Sprintf(`{"auth":%q}`, basicAuth("alice", "s3cret")))
	path := plantConfig(t, planted)
	store := newStore(t, path)

	got, err := store.Credential(noExec(t), fixtureRegistry)

	require.NoError(t, err)
	assert.Equal(t, oci.Credential{Username: "alice", Password: "s3cret"}, got)
	assert.Equal(t, path, store.ConfigPath())
	assert.True(
		t,
		strings.HasPrefix(store.ConfigPath(), filepath.Dir(path)),
		"the store read a file outside the directory this row planted one in",
	)
}

// TestDefaultConfigPathFollowsTheOverride pins the $DOCKER_CONFIG
// short-circuit, which is also what makes that variable a complete
// isolation gate for every suite in this repository.
func TestDefaultConfigPathFollowsTheOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configDirEnv, dir)

	path, err := auth.DefaultConfigPath()

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "config.json"), path)
}

// TestDefaultConfigPathFallsBackToTheHomeDirectory pins where the
// configuration is looked for when no variable names a directory.
func TestDefaultConfigPathFallsBackToTheHomeDirectory(t *testing.T) {
	t.Setenv(configDirEnv, "")

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	path, err := auth.DefaultConfigPath()

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".docker", "config.json"), path)
	assert.Equal(
		t,
		homeDirName,
		filepath.Base(home),
		"the fallback resolved against the real home directory rather than the one this suite installed",
	)
}

// The two rows below share the one directory on the test PATH and both write a
// program called docker-credential-fake into it, so neither runs in parallel:
// they take turns, which is what a top-level test that never calls
// [testing.T.Parallel] does.

func TestStoreCredentialRunsTheHelperTheConfigurationNames(t *testing.T) {
	record := writeHelper(t, "fake", fakeHelperScript)
	store := newStore(t, plantConfig(t, `{"credsStore":"fake"}`))

	got, err := store.Credential(t.Context(), fixtureRegistry)

	require.NoError(t, err)
	assert.Equal(t, oci.Credential{Username: helperUser, Password: helperSecret}, got)
	assert.Equal(
		t,
		"get\n",
		helperRan(t, record),
		"the credential has to have come from the helper, not from somewhere the row cannot see",
	)
}

// TestStoreCredentialDoesNotRunAHelperTheConfigurationDoesNotName is the
// twin of the positive control: same fake, same PATH, no credsStore — the
// helper must stay unexecuted.
func TestStoreCredentialDoesNotRunAHelperTheConfigurationDoesNotName(t *testing.T) {
	record := writeHelper(t, "fake", fakeHelperScript)
	config := authsConfig(string(fixtureRegistry), fmt.Sprintf(`{"auth":%q}`, basicAuth("alice", "s3cret")))
	store := newStore(t, plantConfig(t, config))

	got, err := store.Credential(noExec(t), fixtureRegistry)

	require.NoError(t, err)
	assert.Equal(t, oci.Credential{Username: "alice", Password: "s3cret"}, got)
	assert.Empty(t, helperRan(t, record), "a helper sitting on PATH was run without the configuration naming it")
}

// TestStoreCredentialDoesNotRunThePlatformsOwnHelper pins the
// DetectDefaultNativeStore choice: an empty configuration must not fall
// through to the platform keychain helper, which would read the
// developer's real credentials from a test.
func TestStoreCredentialDoesNotRunThePlatformsOwnHelper(t *testing.T) {
	records := make(map[string]string, len(platformHelpers()))
	for _, name := range platformHelpers() {
		records[name] = writeHelper(t, name, fakeHelperScript)
	}

	store := newStore(t, plantConfig(t, `{}`))

	got, err := store.Credential(noExec(t), fixtureRegistry)

	require.NoError(t, err)
	assert.True(t, got.Empty())

	for name, record := range records {
		assert.Empty(
			t,
			helperRan(t, record),
			"an empty configuration reached the platform's own credential store through %s, "+
				"which on a developer's machine is their real keychain",
			name,
		)
	}
}

// TestStoreCredentialGivesUpOnAHelperThatNeverAnswers pins the lookup
// bound: a wedged helper costs one bounded wait, never a hung transfer.
func TestStoreCredentialGivesUpOnAHelperThatNeverAnswers(t *testing.T) {
	t.Parallel()

	record := writeHelper(t, "wedged", wedgedHelperScript)
	store := newStore(t, plantConfig(t, `{"credsStore":"wedged"}`))

	started := time.Now()
	got, err := store.Credential(t.Context(), fixtureRegistry)
	took := time.Since(started)

	require.Error(t, err)
	assert.True(t, got.Empty())
	assert.NotEmpty(t, helperRan(t, record), "the helper never ran, so this row proved nothing about waiting for one")
	assert.Less(t, took, wedgedHelperBudget, "the lookup waited for the helper instead of bounding it")
	assert.Contains(t, err.Error(), "10s", "the failure has to name the limit it ran out of")
	assert.Contains(t, err.Error(), string(fixtureRegistry), "the failure has to name the registry it was looking up")
}

// noExec returns a context that fails the test if a credential helper runs
// under it.
//
// It is what turns "this row did not need a helper" into something proved
// rather than assumed. Every row that reads a configuration file and nothing
// else carries it, so a change that made an ordinary lookup shell out —
// through a platform default, a fallback, a helper named somewhere
// unexpected — fails here instead of quietly reaching a developer's keychain
// on their machine and nothing on the build's.
func noExec(t *testing.T) context.Context {
	t.Helper()

	return trace.WithExecutableTrace(t.Context(), &trace.ExecutableTrace{
		ExecuteStart: func(program, action string) {
			t.Fatalf("the lookup ran %s (%s); this row must not run a program", program, action)
		},
	})
}

// newStore returns a store reading the configuration at path.
func newStore(t *testing.T, path string) *auth.Store {
	t.Helper()

	store, err := auth.NewStore(path)
	require.NoError(t, err)

	return store
}

// plantConfig writes config into a directory of this test's own and returns
// the path, pinned by [pin] so nothing the row does can change it.
func plantConfig(t *testing.T, config string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(config), configPerm))

	pin(t, path)

	return path
}

// pin records what the configuration file holds and asserts, once the test has
// finished, that it holds exactly the same thing.
//
// The port bigoci looks credentials up through has one read method, so a write
// is unrepresentable rather than merely unlikely — but the store underneath it
// can write, and this is what keeps the difference between those two
// statements honest. A file that was not there has to still not be there.
func pin(t *testing.T, path string) {
	t.Helper()

	before, err := os.ReadFile(path)
	missing := errors.Is(err, os.ErrNotExist)

	if !missing {
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		after, err := os.ReadFile(path)
		if missing {
			assert.ErrorIs(t, err, os.ErrNotExist, "the lookup created a Docker configuration")

			return
		}

		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "the lookup wrote to the Docker configuration")
	})
}

// writeHelper writes a credential helper program called
// docker-credential-<name> into the one directory on the test PATH, and
// returns the file that program records its invocations in.
//
// The program is a shell script, which is why these rows do not run on
// Windows. What they prove — that a helper runs when the configuration names
// one and never otherwise — is about the store's own decision rather than
// about how a program is spelled on a platform.
func writeHelper(t *testing.T, name, script string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the credential helper rows run a shell script")
	}

	record := filepath.Join(t.TempDir(), "invocations")
	program := filepath.Join(os.Getenv(pathEnv), "docker-credential-"+name)

	require.NoError(t, os.WriteFile(program, fmt.Appendf(nil, script, record), helperPerm))
	t.Cleanup(func() {
		assert.NoError(t, os.Remove(program))
	})

	return record
}

// helperRan returns what the helper at record wrote down, and the empty string
// when it never ran at all.
func helperRan(t *testing.T, record string) string {
	t.Helper()

	ran, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}

	require.NoError(t, err)

	return string(ran)
}

// platformHelpers names the credential helpers a store would reach for if it
// detected the platform's own, which is the fallback bigoci turns off. Linux
// picks between two depending on what is installed, so both are planted.
func platformHelpers() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"osxkeychain"}
	case "windows":
		return []string{"wincred"}
	default:
		return []string{"pass", "secretservice"}
	}
}

// authsConfig renders a Docker configuration holding one auths entry: key is
// the server address it is filed under and entry is the JSON object stored
// there.
func authsConfig(key, entry string) string {
	return fmt.Sprintf(`{"auths":{%q:%s}}`, key, entry)
}

// basicAuth renders the base64 "user:secret" that a configuration entry's auth
// field carries, which is how `docker login` stores a password.
func basicAuth(username, secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + secret))
}
