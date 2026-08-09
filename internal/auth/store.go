package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"oras.land/oras-go/v2/registry/remote/credentials"

	"github.com/componere/bigoci/internal/oci"
)

// credLookupTimeout is how long one credential lookup may take.
//
// A lookup usually reads a file and returns in microseconds, but a
// configuration that names a credential helper turns it into running someone
// else's program, and a program can hang. The bound is a constant rather than
// a knob: a transfer's deadline belongs to the caller's context, and this is
// only here so a wedged helper fails a transfer instead of stopping it
// forever.
const credLookupTimeout = 10 * time.Second

// Where the Docker command line keeps its configuration, which is where
// bigoci looks for it too.
const (
	// configDirEnv overrides the directory the configuration file lives in.
	configDirEnv = "DOCKER_CONFIG"
	// configDirName is the directory under the user's home that holds it
	// otherwise.
	configDirName = ".docker"
	// configFileName is the file itself.
	configFileName = "config.json"
)

// Store resolves credentials out of a Docker configuration file, the same way
// the Docker command line does: the entry under the registry's server
// address, or whatever the credential helper the file names prints for it.
//
// A Store never writes. It is built from one file path, which it reads once
// and keeps the parse of, and it is safe to use from several goroutines at
// once. A lookup either answers out of that parse or runs the credential
// helper the file named, which is asked afresh every time.
//
// A lookup may execute a credential helper program. The package
// documentation says what that means and why bigoci does it.
type Store struct {
	// store is oras-go's dynamic store: it reads the configuration file and,
	// where the file says to, runs the credential helper named there. It is
	// the one piece of oras-go bigoci uses, because reading this file
	// faithfully — the auths keys, the base64 auth field, the helper
	// protocol, Docker Hub's server address — is a compatibility surface
	// rather than a feature.
	store *credentials.DynamicStore
}

// NewStore returns the credential source described by the Docker
// configuration file at path.
//
// A file that is not there is not an error: it is a machine nobody has run
// `docker login` on, which resolves every registry to the anonymous
// credential. A file that is there and cannot be read as a configuration is
// an error, and it is worth failing on — a caller who asked bigoci to use
// their credentials would otherwise watch it transfer anonymously and fail
// somewhere less obvious.
//
// The store is built with plaintext writes disabled and platform-store
// detection off. Neither is reachable through the port — it has one read
// method — but detection is not a write setting: with it on, a configuration
// file that names no helper falls through to the platform's own, so a lookup
// against an empty file would still execute docker-credential-osxkeychain and
// ask the developer's real keychain. bigoci runs a helper only when the file
// in front of it names one.
func NewStore(path string) (*Store, error) {
	store, err := credentials.NewStore(path, credentials.StoreOptions{
		AllowPlaintextPut:        false,
		DetectDefaultNativeStore: false,
	})
	if err != nil {
		return nil, fmt.Errorf("read the Docker configuration at %s: %w", path, err)
	}

	return &Store{store: store}, nil
}

// DefaultConfigPath returns the Docker configuration file bigoci reads when a
// caller names none: $DOCKER_CONFIG/config.json when that variable is set, and
// the .docker directory under the user's home otherwise — $HOME on Unix,
// %USERPROFILE% on Windows.
//
// bigoci works the path out itself and hands it to [NewStore] rather than
// letting the store read the environment, so a test can point a store at a
// file of its own without changing a variable the whole process shares. That
// separation is what lets the credential tests prove they never touched the
// developer's real configuration.
//
// It fails only when there is no home directory to fall back to and no
// variable naming one, which is a machine that cannot say where its
// configuration would be.
func DefaultConfigPath() (string, error) {
	if dir := os.Getenv(configDirEnv); dir != "" {
		return filepath.Join(dir, configFileName), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate the Docker configuration directory: %w", err)
	}

	return filepath.Join(home, configDirName, configFileName), nil
}

// ConfigPath returns the configuration file this store reads.
//
// It is what a caller reports when they need to say where a credential came
// from, and what the tests assert on to prove which file was read.
func (s *Store) ConfigPath() string {
	return s.store.ConfigPath()
}

// Credential returns what the configuration holds for registry, or the
// anonymous credential when it holds nothing for it.
//
// The lookup key is the registry named by the reference, mapped through the
// one translation Docker's history requires: a login to Docker Hub is stored
// under https://index.docker.io/v1/, while a reference names the registry
// docker.io — or spells out the dialed host registry-1.docker.io — so a
// credential written by `docker login` would otherwise be invisible to the
// registry it belongs to. [serverAddress] covers both spellings. Nothing
// else about a registry is rewritten, and the key is always the registry the
// reference named — never a name a registry offered in a challenge.
//
// An identity token comes back in the field of that name rather than being
// folded into the password. bigoci cannot exchange one, and the oci package
// refuses it in as many words; a credential that arrived as ("", "") would be
// indistinguishable from no credential at all and would quietly downgrade the
// transfer to anonymous.
func (s *Store) Credential(ctx context.Context, registry oci.Registry) (oci.Credential, error) {
	lookup, cancel := context.WithTimeout(ctx, credLookupTimeout)
	defer cancel()

	got, err := s.store.Get(lookup, serverAddress(registry))
	if err != nil {
		detail := "the configuration could not answer"
		if lookup.Err() != nil && ctx.Err() == nil {
			detail = fmt.Sprintf("the credential helper did not answer within %s", credLookupTimeout)
		}

		return oci.Credential{}, &lookupError{
			registry: registry,
			path:     s.ConfigPath(),
			detail:   detail,
			cause:    err,
		}
	}

	return oci.Credential{
		Username:      got.Username,
		Password:      got.Password,
		IdentityToken: got.RefreshToken,
		RegistryToken: got.AccessToken,
	}, nil
}

// serverAddress maps a registry name onto the key `docker login` stores its
// credential under. Docker Hub is the one registry with history here, and a
// reference can miss its legacy https://index.docker.io/v1/ key from either
// direction: a reference usually names the registry docker.io, which only the
// registry-name mapping knows, while one that spells out the dialed host
// writes registry-1.docker.io, which only the hostname mapping knows. The
// hostname mapping is tried first and the registry-name mapping where it
// changed nothing; every other registry passes through untouched.
func serverAddress(registry oci.Registry) string {
	if mapped := credentials.ServerAddressFromHostname(string(registry)); mapped != string(registry) {
		return mapped
	}

	return credentials.ServerAddressFromRegistry(string(registry))
}

// lookupError reports that the configuration could not answer a credential
// lookup, without repeating what the underlying store said. The store's
// message can carry the decoded content of a malformed auth entry — the
// secret itself, for an entry holding a bare token — and this error reaches
// terminals and CI logs verbatim, so it names only the registry, the file,
// and what kind of failure it was. The cause stays reachable through
// [lookupError.Unwrap] for [errors.Is] and [errors.As].
type lookupError struct {
	// registry is the registry whose credential was being looked up.
	registry oci.Registry
	// path is the configuration file the store reads.
	path string
	// detail says what kind of failure this was, in words this package chose.
	detail string
	// cause is the store's own error, unwrapped but never rendered.
	cause error
}

// Error names the registry, the configuration file, and the kind of failure —
// and deliberately not the cause's text.
func (e *lookupError) Error() string {
	return fmt.Sprintf("look up the credential for %s in %s: %s", e.registry, e.path, e.detail)
}

// Unwrap returns the store's own error, so a caller can still inspect the
// class of failure without its text reaching a message.
func (e *lookupError) Unwrap() error {
	return e.cause
}
