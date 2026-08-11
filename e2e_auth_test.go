package bigoci_test

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
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/imgoci/bigoci"
)

// The credential the registry in this file accepts, and the file it reads it
// out of.
const (
	// authUser is the account name the registry knows.
	authUser = "bigoci"
	// authPassword is that account's password.
	authPassword = "hunter2"
	// authPasswordHash is bcrypt(authPassword), committed beside the plaintext
	// it belongs to.
	//
	// It is a constant rather than something the test computes, which keeps
	// golang.org/x/crypto out of the module for the sake of a fixture. The
	// cost parameter is 4, bcrypt's minimum: zot verifies the hash on every
	// request it answers, and at the usual cost of 10 a multi-part transfer
	// would spend seconds doing nothing but that.
	authPasswordHash = `$2b$04$DbgsCUisFL9WAp2vA9vzgeZHDwapS61A6/SXjYvARyxPPvTRniSTa`
	// authWrongPassword is a password the registry does not know.
	authWrongPassword = "not-the-password"
	// authOtherHost is a registry nothing in these tests dials. A credential
	// stored under it must never be presented anywhere else.
	authOtherHost = "registry.example.com"
	// authRepo is the repository these tests move artifacts through.
	authRepo = "e2e/auth"
)

// Where the Docker configuration lives, spelled the way the library's own
// lookup spells it.
const (
	// dockerConfigEnv points the credential lookup at the directory holding
	// the configuration file.
	dockerConfigEnv = "DOCKER_CONFIG"
	// dockerConfigName is the configuration file inside that directory.
	dockerConfigName = "config.json"
)

// The registry fixture that demands a credential, which is one configuration
// field and one file more than the plain one.
const (
	// htpasswdPath is where the registry's configuration says the password
	// file is.
	htpasswdPath = "/etc/zot/htpasswd"
	// htpasswdMode is the mode it is written with: readable by everyone, which
	// is all the registry needs.
	htpasswdMode = 0o644
	// zotAuthConfig is zotConfig with the htpasswd block added and nothing
	// else changed. A registry configured this way answers every request
	// without a credential with 401 and a Basic challenge, including the one
	// the readiness check makes.
	zotAuthConfig = `{
  "storage": {"rootDirectory": "/var/lib/registry"},
  "http": {
    "address": "0.0.0.0",
    "port": "5000",
    "auth": {"htpasswd": {"path": "/etc/zot/htpasswd"}}
  },
  "log": {"level": "error"}
}`
)

// refusedRequests is how many requests one operation makes against a registry
// that refuses the credential it presented: the request itself, and the single
// re-issue the challenge bought.
//
// It is the number that says nothing was burned. A refusal that went through
// the retry policy would make four times as many requests and take three
// waits, and a credential refreshed and presented again would make more still.
const refusedRequests = 2

// TestE2EAuthRefusesAClientWithNoCredential is the negative control the rest of
// this file rests on, and it is first because everything below is vacuous
// without it.
//
// A registry that answered these transfers would be a registry with
// authentication switched off, and the rows that follow — including the ones
// that prove a credential works — would then be proving only that a transfer
// works. A failure here is a statement about the fixture, not about the
// library.
func TestE2EAuthRefusesAClientWithNoCredential(t *testing.T) {
	reg := newAuthZot(t)

	t.Run("a client that was given no credential source at all", func(t *testing.T) {
		assertRefused(t, reg, newClient(t, bigoci.WithPlainHTTP()))
	})

	t.Run("a client reading a Docker configuration that is not there", func(t *testing.T) {
		// An empty directory holds no config.json, which is a machine nobody
		// has run `docker login` on. That is not a failure to build a client —
		// it resolves every registry to the anonymous credential — and this
		// row is where both halves of that are checked.
		t.Setenv(dockerConfigEnv, t.TempDir())

		assertRefused(t, reg, newClient(t, bigoci.WithPlainHTTP(), bigoci.WithDockerCredentials()))
	})
}

// TestE2EAuthRefusesTheWrongPasswordWithoutBurningAnything checks what a
// refusal costs as well as what it reports.
//
// A wrong password is not worth a second attempt: no wait, no retry, and above
// all no blob bytes on the wire, because a registry that will not take the
// credential will not take the part either. The request counts are what say so.
func TestE2EAuthRefusesTheWrongPasswordWithoutBurningAnything(t *testing.T) {
	reg := newAuthZot(t)

	t.Run("a push stops at its first request and sends no blob", func(t *testing.T) {
		front, log := newRecordingProxy(t, reg.host)
		client := newRefusedClient(t, front.host)

		_, err := client.Push(
			t.Context(), front.taggedRef(authRepo, tag), bigoci.FromFile(newRandomFile(t, multiSize)),
			bigoci.WithPartSize(multiPartSize), bigoci.WithWorkers(1),
		)

		require.ErrorIs(t, err, bigoci.ErrUnauthorized)
		assert.Equal(t, refusedRequests, log.total(), "a refused push must cost the challenge and one re-issue")
		assert.Zero(t, log.count(classUploadOpen), "a refused push must open no upload session")
		assert.Zero(t, log.count(classUploadComplete), "a refused push must send no blob bytes")
	})

	t.Run("a pull stops at the manifest", func(t *testing.T) {
		front, log := newRecordingProxy(t, reg.host)
		client := newRefusedClient(t, front.host)
		dest := newPath(t, destName)

		err := client.Pull(t.Context(), front.taggedRef(authRepo, tag), bigoci.ToFile(dest), bigoci.WithWorkers(1))

		require.ErrorIs(t, err, bigoci.ErrUnauthorized)
		assert.Equal(t, refusedRequests, log.total(), "a refused pull must cost the challenge and one re-issue")
		assert.Zero(t, log.count(classBlobGet), "a pull that never read a manifest must read no part")
		assert.NoFileExists(t, dest, "a refused pull publishes nothing")
	})
}

// TestE2EAuthMovesFilesWithACredential is the positive gate: the same registry
// that refused everything above serves a whole multi-part transfer once bigoci
// has a credential for it.
func TestE2EAuthMovesFilesWithACredential(t *testing.T) {
	reg := newAuthZot(t)

	t.Run("the credential a docker login left behind", func(t *testing.T) {
		useDockerConfig(t, reg.host, authPassword)

		client := newClient(t, bigoci.WithPlainHTTP(), bigoci.WithDockerCredentials())
		source := newRandomFile(t, multiSize)
		dest := newPath(t, destName)

		_, err := client.Push(
			t.Context(), reg.taggedRef(authRepo, tag), bigoci.FromFile(source), bigoci.WithPartSize(multiPartSize),
		)
		require.NoError(t, err)
		require.NoError(t, client.Pull(t.Context(), reg.taggedRef(authRepo, tag), bigoci.ToFile(dest)))

		assert.Equal(t, fileDigest(t, source), fileDigest(t, dest), "the pulled file must be byte-identical")
	})

	t.Run("a tagless digest reference round trips", func(t *testing.T) {
		const repo = "e2e/auth-digest"

		useDockerConfig(t, reg.host, authPassword)

		client := newClient(t, bigoci.WithPlainHTTP(), bigoci.WithDockerCredentials())
		source := newRandomFile(t, multiSize)
		dest := newPath(t, destName)

		desc, err := client.Push(
			t.Context(), reg.taggedRef(repo, tag), bigoci.FromFile(source), bigoci.WithPartSize(multiPartSize),
		)
		require.NoError(t, err)
		require.NoError(t, client.Pull(t.Context(), reg.digestRef(repo, desc.Digest), bigoci.ToFile(dest)))

		assert.Equal(t, fileDigest(t, source), fileDigest(t, dest))
	})

	t.Run("a credential the caller passed straight in", func(t *testing.T) {
		const repo = "e2e/auth-direct"

		// No configuration file anywhere: this is the option for a caller who
		// already holds the secret, and the empty directory is what proves the
		// credential came from the argument.
		t.Setenv(dockerConfigEnv, t.TempDir())

		client := newClient(t, bigoci.WithPlainHTTP(), bigoci.WithCredentials(authUser, authPassword))
		source := newRandomFile(t, multiSize)
		dest := newPath(t, destName)

		_, err := client.Push(
			t.Context(), reg.taggedRef(repo, tag), bigoci.FromFile(source), bigoci.WithPartSize(multiPartSize),
		)
		require.NoError(t, err)
		require.NoError(t, client.Pull(t.Context(), reg.taggedRef(repo, tag), bigoci.ToFile(dest)))

		assert.Equal(t, fileDigest(t, source), fileDigest(t, dest))
	})
}

// TestE2EAuthIgnoresACredentialStoredForAnotherHost checks the lookup key.
//
// The credential is the right one, spelled correctly, stored under a host this
// transfer never dials. A lookup keyed on anything but the host bigoci dialed
// — the service name a challenge offers, say — would find it and present it,
// which is a secret leaving the machine for a registry that never asked for
// it. The refusal is the proof it stayed put.
func TestE2EAuthIgnoresACredentialStoredForAnotherHost(t *testing.T) {
	reg := newAuthZot(t)
	useDockerConfig(t, authOtherHost, authPassword)

	client := newClient(t, bigoci.WithPlainHTTP(), bigoci.WithDockerCredentials())

	assertRefused(t, reg, client)
}

// authRecord is one request a recording fixture carried.
type authRecord struct {
	// class is what the request was for, in the vocabulary
	// [classifyRequest] sorts registry traffic into.
	class string
	// dgst is the blob the request names, empty for a request that names none.
	dgst digest.Digest
}

// authLog is what a fixture standing in front of a registry saw go past.
//
// It counts rather than records bytes: every row here asks how many requests
// one operation cost, or how many attempts one part drew, and neither question
// needs the traffic itself.
type authLog struct {
	// mu guards seen, which the fixture's handler writes from whichever
	// goroutine is serving a request.
	mu sync.Mutex
	// seen is every request that went past, in arrival order.
	seen []authRecord
}

// record notes one request on its way past.
func (l *authLog) record(req *http.Request) {
	class, dgst := classifyRequest(req)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.seen = append(l.seen, authRecord{class: class, dgst: dgst})
}

// total returns how many requests went past, whatever they were for.
func (l *authLog) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.seen)
}

// count returns how many of them were of one class.
func (l *authLog) count(class string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	total := 0

	for _, one := range l.seen {
		if one.class == class {
			total++
		}
	}

	return total
}

// most returns the largest number of requests of one class that any single
// blob drew, which is how a row says no part was attempted more often than its
// budget allows.
func (l *authLog) most(class string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	per := make(map[digest.Digest]int)
	worst := 0

	for _, one := range l.seen {
		if one.class != class || one.dgst == "" {
			continue
		}

		per[one.dgst]++
		worst = max(worst, per[one.dgst])
	}

	return worst
}

// newAuthZot starts a zot that demands a credential and returns the address it
// answers on.
//
// It is the ordinary fixture with two files replaced: a configuration naming
// an htpasswd file, and the htpasswd file itself. The readiness check has to
// be told to expect a refusal, because a registry that is up and enforcing
// authentication answers the version endpoint with 401 — which is also the
// first proof that the fixture is configured the way this file needs.
func newAuthZot(t *testing.T) zot {
	t.Helper()

	return newZot(t,
		testcontainers.WithFiles(
			testcontainers.ContainerFile{
				Reader:            strings.NewReader(zotAuthConfig),
				ContainerFilePath: zotConfigPath,
				FileMode:          zotConfigMode,
			},
			testcontainers.ContainerFile{
				Reader:            strings.NewReader(authUser + ":" + authPasswordHash + "\n"),
				ContainerFilePath: htpasswdPath,
				FileMode:          htpasswdMode,
			},
		),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP(apiPath).WithPort(zotPort).WithStatusCodeMatcher(func(status int) bool {
				return status == http.StatusUnauthorized
			}),
		),
	)
}

// newRecordingProxy returns a registry address in front of upstream that
// forwards every request untouched and counts what went past.
//
// Counting outside the library is what makes a count worth asserting on: the
// numbers are what a registry saw, not what bigoci believes it did. The
// credential travels through it unread — a reverse proxy forwards the
// Authorization header — so a row that stands one of these in front of the
// registry still measures the real exchange.
func newRecordingProxy(t *testing.T, upstream string) (zot, *authLog) {
	t.Helper()

	origin, err := url.Parse("http://" + upstream)
	require.NoError(t, err)

	log := &authLog{}
	proxy := httputil.NewSingleHostReverseProxy(origin)
	proxy.Transport = newTransport(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		log.record(req)
		proxy.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)

	at := zot{host: strings.TrimPrefix(server.URL, "http://")}
	t.Logf("a recording proxy on %s is serving %s in front of %s", at.host, apiPath, upstream)

	return at, log
}

// newRefusedClient returns a client carrying the right account name and the
// wrong password for host, read the way a real one would be.
func newRefusedClient(t *testing.T, host string) *bigoci.Client {
	t.Helper()

	useDockerConfig(t, host, authWrongPassword)

	return newClient(t, bigoci.WithPlainHTTP(), bigoci.WithDockerCredentials())
}

// useDockerConfig writes the Docker configuration a login to host as the
// registry's one account would have left behind, and points $DOCKER_CONFIG at
// the directory holding it for the length of the test.
//
// The file is the one `docker login` writes, in the format it writes it, which
// is the point of the row: what is being tested is that bigoci reads what a
// login left behind, not that it reads a shape invented here. The account name
// never varies, because the registry knows one; what a row chooses is the
// secret beside it and the host it is filed under.
func useDockerConfig(t *testing.T, host, password string) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			host: map[string]string{
				"auth": base64.StdEncoding.EncodeToString([]byte(authUser + ":" + password)),
			},
		},
	})
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, dockerConfigName), body, fixturePerm))
	t.Setenv(dockerConfigEnv, dir)
}

// assertRefused checks that both directions come back as the unauthorized
// sentinel, and that neither published anything on the way.
func assertRefused(t *testing.T, reg zot, client *bigoci.Client) {
	t.Helper()

	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(), reg.taggedRef(authRepo, tag), bigoci.FromFile(newRandomFile(t, smallSize)),
		bigoci.WithPartSize(multiPartSize),
	)
	require.ErrorIs(t, err, bigoci.ErrUnauthorized, "a push the registry refused")

	err = client.Pull(t.Context(), reg.taggedRef(authRepo, tag), bigoci.ToFile(dest))
	require.ErrorIs(t, err, bigoci.ErrUnauthorized, "a pull the registry refused")
	assert.NoFileExists(t, dest, "a refused pull publishes nothing")
}
