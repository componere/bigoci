//go:build conformance

// This file is compiled only under the "conformance" build tag. An ordinary
// build, `go test ./...`, and every per-commit CI run leave it out entirely,
// because everything in it talks to a real cloud registry with a real
// credential and costs real money and time. The workflow that runs it —
// .github/workflows/conformance.yml — is hand-triggered and nothing else
// triggers it.
//
// What it exists to prove is the part no fake can: that the bearer dance, the
// presigned redirect, and the refusals behave the same way against a registry
// nobody wrote for the occasion. The gates it runs are the ones cli/README.md
// spells out as shell recipes, in the order the phase's design fixed them,
// with the negative control first because every row under it is vacuous
// without it.

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The environment this suite is pointed at. Nothing here has a default: a
// conformance run is a deliberate act against a named registry, and guessing
// one would be a way to push to somebody's repository by accident.
const (
	// repoEnv names the repository every row transfers to, written the way a
	// reference writes it with no tag: "ghcr.io/imgoci/bigoci/conformance".
	// Unset means there is no registry to run against and the suite skips.
	repoEnv = "BIGOCI_CONFORMANCE_REPO"
	// credentialEnv names the directory holding the config.json the
	// authenticated rows use.
	//
	// It is deliberately not DOCKER_CONFIG. [TestMain] empties that variable
	// before any test in this package runs, so that no other test can reach a
	// real credential whatever machine it runs on. This suite is the one test
	// that needs one, so it is handed the directory under its own name and
	// points DOCKER_CONFIG at it for the length of a row.
	credentialEnv = "BIGOCI_CONFORMANCE_DOCKER_CONFIG"
	// logDirEnv names a directory the captured request logs are written into,
	// which is what the workflow uploads as the run's evidence. Unset means the
	// logs stay in the test's own output.
	logDirEnv = "BIGOCI_CONFORMANCE_LOG_DIR"
	// runIDEnv is what a GitHub Actions run calls itself.
	runIDEnv = "GITHUB_RUN_ID"
	// attemptEnv distinguishes a re-run from the run it repeats. The run id
	// alone does not: a re-run carries the same one.
	attemptEnv = "GITHUB_RUN_ATTEMPT"
)

const (
	// conformanceSize is how large the fixture is, chosen so the push is
	// unmistakably a multi-part one and the pull has enough blob reads for the
	// no-leak gate to count. Two mebibytes moves in seconds over any link a CI
	// runner has.
	conformanceSize = 2 * mib
	// conformancePartSize is the part size the pushes ask for, spelled the way
	// a caller types it.
	conformancePartSize = "256KiB"
	// conformancePartBytes is that same size as a number. The two must agree:
	// the part count below is derived from this one and is what the no-leak
	// gate counts off-registry requests against.
	conformancePartBytes = 256 * kib
	// conformanceParts is how many parts conformanceSize splits into, and so
	// how many blobs a pull of it reads.
	conformanceParts = conformanceSize / conformancePartBytes
	// conformanceTimeout bounds each transfer.
	//
	// It is generous for the size being moved, and its job is not to be a
	// deadline: it is what turns a wedged transfer into a run that ends by
	// itself with its log already written, rather than into a `go test`
	// timeout that panics the binary and loses the evidence. The arithmetic
	// that keeps that true lives in two places: six CLI runs at this budget
	// is 24 minutes, and the workflow's binary timeout is 30 — change either
	// and re-check the other.
	conformanceTimeout = "4m"
	// logMode is the mode a captured log is written with.
	logMode = 0o600
	// logDirMode is the mode the directory holding them is created with.
	logDirMode = 0o700
	// requestLineFields is the smallest number of whitespace-separated fields
	// an "http> " line of the frozen -debug grammar has before its header
	// fields begin.
	requestLineFields = 7
	// urlField, classField, and authField are where those three sit in a
	// request line, counting the "http> " prefix as the first field.
	urlField   = 4
	classField = 5
	authField  = 6
)

// conformance is what one run of this suite was pointed at.
type conformance struct {
	// repo is the repository every row transfers to, with no tag.
	repo string
	// registry is the host half of repo. It is the key the no-leak gate reads:
	// a request to any other host has left the registry.
	registry string
	// ref is the reference the round trip pushes to and both refused rows name.
	ref string
	// taglessRef is the reference the digest round trip pushes to before
	// pulling the manifest back by digest alone.
	taglessRef string
	// credentials is the directory holding the config.json the authenticated
	// rows point DOCKER_CONFIG at.
	credentials string
}

// TestConformance drives the CLI against a real cloud registry.
//
// The rows run in order and the order is the argument. The negative control
// comes first: a registry that would serve this repository to a caller
// carrying nothing cannot show that anything below it carried a credential, so
// a control that fails ends the run rather than letting five green rows say
// nothing. What follows is the round trip, the counted no-leak gate over the
// log that round trip captured, the digest-only round trip, and the credential
// the registry refuses.
func TestConformance(t *testing.T) {
	c := newConformance(t)

	if !t.Run("a pull with no credential is refused", c.refusesAnAnonymousPull) {
		t.Fatal(
			"the negative control failed, so every row below it would prove nothing: " +
				"a repository that answers a caller carrying no credential cannot demonstrate that one reached it",
		)
	}

	var pullLog string
	t.Run("a multi-part push and pull round trip byte for byte", func(t *testing.T) {
		pullLog = c.roundTrips(t)
	})

	t.Run("no credential leaves the registry for storage", func(t *testing.T) {
		// Keyed on the log rather than on whether the row above passed: a pull
		// that finished and delivered the wrong bytes still says everything
		// there is to say about what it sent to storage.
		if pullLog == "" {
			t.Skip("the round trip above did not finish, so it captured no pull log to read")
		}

		c.leaksNothingOffRegistry(t, pullLog)
	})

	t.Run("a digest reference round trips", c.roundTripsADigestReference)
	t.Run("a credential the registry refuses is exit 6", c.refusesAWrongCredential)
}

// newConformance reads the environment this run was pointed at, skipping the
// whole suite when nothing pointed it anywhere.
//
// A repository named with no credential directory beside it is not a skip but
// a failure: it is a run that would meet its negative control, pass it for the
// wrong reason, and then fail every row after it.
func newConformance(t *testing.T) conformance {
	t.Helper()

	repo := os.Getenv(repoEnv)
	if repo == "" {
		t.Skipf(
			"%s is unset, so there is no registry to run against; "+
				"set it to a repository you own, as in %s=ghcr.io/you/bigoci/conformance",
			repoEnv, repoEnv,
		)
	}

	credentials := os.Getenv(credentialEnv)
	require.NotEmptyf(
		t, credentials,
		"%s names a repository but %s names no directory holding a config.json, "+
			"so every row that has to authenticate would be refused",
		repoEnv, credentialEnv,
	)

	registry, _, found := strings.Cut(repo, "/")
	require.Truef(t, found, "%s must name a registry and a repository on it, got %q", repoEnv, repo)

	tag := runTag()

	return conformance{
		repo:        repo,
		registry:    registry,
		ref:         repo + ":" + tag,
		taglessRef:  repo + ":" + tag + "-tagless",
		credentials: credentials,
	}
}

// refusesAnAnonymousPull is the negative control, and it runs before anything
// has been pushed.
//
// It points DOCKER_CONFIG at a directory holding nothing, which is the same
// thing a caller does from a shell with `DOCKER_CONFIG=$(mktemp -d)`, and asks
// for the reference this run is about to write. The refusal is what makes the
// rows below it mean something: they differ from this one only in the
// directory the environment names.
func (c conformance) refusesAnAnonymousPull(t *testing.T) {
	t.Setenv(dockerConfigEnv, t.TempDir())

	dest := filepath.Join(t.TempDir(), "denied.bin")
	got := conformanceRun(t, "01-no-credential.log", cmdPull, "-timeout", conformanceTimeout, "-debug", c.ref, dest)

	require.Equalf(
		t, exitUnauthorized, got.code,
		"a pull carrying no credential must be refused.\n"+
			"Exit %d means the registry told an anonymous caller that %s holds nothing, "+
			"which it only does for a caller allowed to read it.\n"+
			"Exit %d means it served the artifact outright.\n"+
			"Either way %s is readable by anyone and this suite proves nothing about credentials.\n%s",
		exitNotFound, c.ref, exitOK, c.repo, got.stderr,
	)
	assert.Contains(t, got.stderr, "bigoci: matched sentinel bigoci.ErrUnauthorized (exit 6)\n")
	assert.Empty(t, got.stdout, "a transfer the registry refused writes no digest")
	assert.NoFileExists(t, dest, "a refused pull publishes no file")
}

// roundTrips pushes a multi-part file and pulls it back, and returns the pull's
// request log for the gate that reads it.
//
// The part size is small and the file is not, so the push really opens eight
// upload sessions and the pull really reads eight blobs: a single-part transfer
// would exercise none of the concurrency and give the no-leak gate one line to
// count.
func (c conformance) roundTrips(t *testing.T) string {
	t.Setenv(dockerConfigEnv, c.credentials)

	source := conformanceFixture(t)
	dest := filepath.Join(t.TempDir(), "pulled.bin")

	push := conformanceRun(
		t, "02-push.log", cmdPush,
		"-part-size", conformancePartSize, "-timeout", conformanceTimeout, "-debug", source, c.ref,
	)
	require.Equal(t, exitOK, push.code, push.stderr)

	manifest := strings.TrimSuffix(push.stdout, "\n")
	require.Truef(t, isDigest(manifest), "a push writes exactly one manifest digest, got %q", push.stdout)

	pull := conformanceRun(t, "02-pull.log", cmdPull, "-timeout", conformanceTimeout, "-debug", c.ref, dest)
	require.Equal(t, exitOK, pull.code, pull.stderr)

	assert.Equal(t, fileDigest(t, source), fileDigest(t, dest), "a round trip must be byte-identical")

	return pull.stderr
}

// leaksNothingOffRegistry is the counted no-leak gate, read off the log the
// round trip above captured rather than off a transfer that worked.
//
// A working pull is not evidence: signed object storage answers a request that
// also carries the registry's bearer token exactly as happily as a clean one,
// which is measured of both GHCR and Docker Hub. The log is the evidence, and
// three counts have to hold together:
//
//   - Presence. At least one request per part left the registry. Zero is a
//     pull that never followed a redirect, and then every line below is about
//     nothing — so it fails loudly rather than passing.
//   - Universality. Every request that left carried no credential at all. The
//     key is the host and the auth field, never the class: what a storage URL's
//     path looks like is the storage provider's choice.
//   - The instrument saw traffic. There is a token exchange in the log and at
//     least one request that did carry a bearer, so "everything reads
//     auth=none" cannot pass on a log where nothing ever authenticated.
//
// The host key holds because the first registry origin remains literal and
// every off-origin target becomes the reserved off-origin.invalid placeholder.
// GHCR's token realm is on the registry itself. Against a registry whose realm
// lives on another host, the exchange would count as an off-registry request
// carrying a credential, and the gate would need re-deriving rather than
// re-pointing.
func (c conformance) leaksNothingOffRegistry(t *testing.T, pullLog string) {
	lines := parseRequestLines(t, pullLog)
	require.NotEmpty(t, lines, "the pull log holds no request lines at all")

	// The challenge count reads the response lines raw: challenge="present" on
	// an http< line is the registry stating its requirement. Counting the field
	// name alone would be vacuous because frozen grammar prints challenge=- on
	// every response without one.
	challenges := 0
	for raw := range strings.SplitSeq(pullLog, "\n") {
		if strings.HasPrefix(raw, "http< ") && strings.Contains(raw, `challenge="present"`) {
			challenges++
		}
	}

	var offRegistry, carried []string
	exchanges, bearers := 0, 0

	for _, line := range lines {
		if line.class == string(classOther) {
			exchanges++
		}
		if line.auth == authBearer {
			bearers++
		}
		if line.host == c.registry {
			continue
		}

		offRegistry = append(offRegistry, line.text)
		if line.auth != authNone {
			carried = append(carried, line.text)
		}
	}

	require.NotEmptyf(
		t, offRegistry,
		"no redirect observed; the no-leak gate is vacuous against this registry: "+
			"every request in the pull log went to %s, so nothing here says what bigoci sends to storage",
		c.registry,
	)
	assert.GreaterOrEqualf(
		t, len(offRegistry), conformanceParts,
		"a pull of %d parts must leave the registry at least once per part, got %d off-registry requests",
		conformanceParts, len(offRegistry),
	)
	assert.Emptyf(
		t, carried,
		"every request that left %s must carry no credential, and these did:\n%s",
		c.registry, strings.Join(carried, "\n"),
	)
	assert.Positivef(
		t, exchanges,
		"no class=%s request line in the log, so no token was ever exchanged and the tap is not watching "+
			"the authenticated path", classOther,
	)
	assert.Positive(
		t, challenges,
		`no challenge="present" on any response line, so nothing here proves the class=other traffic was a token `+
			"exchange answering the registry's own demand",
	)
	assert.Positive(
		t, bearers,
		"no request in the log carried a bearer token, so a log of nothing but auth=none proves nothing",
	)
}

// roundTripsADigestReference is the tagless row: a manifest fetched back by
// digest alone, with no tag involved in the pull.
//
// Pulling by digest is also what makes the library check the manifest it
// fetched against the digest asked for, so this row exercises the binding as
// well as the reference grammar.
func (c conformance) roundTripsADigestReference(t *testing.T) {
	t.Setenv(dockerConfigEnv, c.credentials)

	source := conformanceFixture(t)
	dest := filepath.Join(t.TempDir(), "tagless.bin")

	push := conformanceRun(
		t, "04-tagless-push.log", cmdPush,
		"-part-size", conformancePartSize, "-timeout", conformanceTimeout, "-debug", source, c.taglessRef,
	)
	require.Equal(t, exitOK, push.code, push.stderr)

	manifest := strings.TrimSuffix(push.stdout, "\n")
	require.Truef(t, isDigest(manifest), "a push writes exactly one manifest digest, got %q", push.stdout)

	pull := conformanceRun(
		t, "04-tagless-pull.log", cmdPull,
		"-timeout", conformanceTimeout, "-debug", c.repo+"@"+manifest, dest,
	)
	require.Equal(t, exitOK, pull.code, pull.stderr)

	assert.Equal(t, fileDigest(t, source), fileDigest(t, dest), "a digest round trip must be byte-identical")
}

// refusesAWrongCredential checks the other half of the refusal contract: a
// credential that is present and wrong is exit 6, the same as none at all.
//
// Nothing here counts requests or waits. How many times a registry lets a
// caller try, and how quickly it answers, is the registry's business; what the
// library owes is the sentinel and the exit code, and those are what this row
// reads. The library's own end-to-end suite is where the no-burn count is
// asserted, against a peer whose behavior it controls.
func (c conformance) refusesAWrongCredential(t *testing.T) {
	useDockerConfig(t, c.registry, gateWrongPassword)

	dest := filepath.Join(t.TempDir(), "refused.bin")
	got := conformanceRun(t, "05-wrong-credential.log", cmdPull, "-timeout", conformanceTimeout, "-debug", c.ref, dest)

	require.Equal(t, exitUnauthorized, got.code, got.stderr)
	assert.Contains(t, got.stderr, "bigoci: matched sentinel bigoci.ErrUnauthorized (exit 6)\n")
	assert.Empty(t, got.stdout, "a transfer the registry refused writes no digest")
	assert.NoFileExists(t, dest, "a refused pull publishes no file")
}

// conformanceRun drives one command line in process and writes what it logged
// where the workflow can pick it up.
//
// The log is written before the caller asserts anything on the result, so a row
// that fails still leaves its evidence behind. On a job nobody can re-run
// cheaply, that is most of what the job is for.
func conformanceRun(t *testing.T, name string, args ...string) result {
	t.Helper()

	got := runCLI(t, args...)
	writeConformanceLog(t, name, got.stderr)

	return got
}

// conformanceFixture writes a file of conformanceSize random bytes and returns
// its path.
//
// The bytes are random rather than a pattern because a pattern would hash to
// blobs the registry already holds from the last run: the push would skip every
// upload, and a row meant to move parts to a cloud registry would move nothing.
func conformanceFixture(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), fixtureName)

	file, err := os.Create(path)
	require.NoError(t, err)

	written, copyErr := io.Copy(file, io.LimitReader(rand.Reader, conformanceSize))
	closeErr := file.Close()
	require.NoError(t, copyErr)
	require.NoError(t, closeErr)
	require.EqualValues(t, conformanceSize, written)

	return path
}

// requestLine is one "http> " line of a captured log, parsed down to the three
// fields the no-leak gate reads.
type requestLine struct {
	// text is the line as it was logged, quoted back into a failure message
	// when the gate rejects it.
	text string
	// host is the rendered target host: the literal registry host, port included
	// when present, or the reserved placeholder for every off-origin target.
	host string
	// class is the value of the line's class field.
	class string
	// auth is the value of the line's auth field: none, bearer, basic, or
	// other.
	auth string
}

// parseRequestLines reads every request line out of a captured -debug log.
//
// The grammar is the frozen one cli/README.md documents:
//
//	http> <seq> <t> <METHOD> <URL> class=<class> auth=<auth> clen=<n> …
//
// so the URL, the class, and the auth scheme are the fifth, sixth, and seventh
// fields. Splitting on runs of whitespace rather than on single spaces is what
// makes that true whatever the method is: the method is padded to four columns,
// so a GET is followed by two spaces and a DELETE by one.
func parseRequestLines(t *testing.T, log string) []requestLine {
	t.Helper()

	var lines []requestLine
	for raw := range strings.Lines(log) {
		line := strings.TrimSuffix(raw, "\n")
		if !strings.HasPrefix(line, "http> ") {
			continue
		}

		fields := strings.Fields(line)
		require.GreaterOrEqualf(t, len(fields), requestLineFields, "a request line has %d fields at least: %s",
			requestLineFields, line)

		target, err := url.Parse(fields[urlField])
		require.NoErrorf(t, err, "the URL of a request line must parse: %s", line)

		lines = append(lines, requestLine{
			text:  line,
			host:  target.Host,
			class: strings.TrimPrefix(fields[classField], "class="),
			auth:  strings.TrimPrefix(fields[authField], "auth="),
		})
	}

	return lines
}

// writeConformanceLog writes one captured request log into the directory the
// workflow uploads, and does nothing at all when no directory was named.
//
// Uploading these is safe by construction rather than by review. The tap logs
// no body in either direction, renders an Authorization header as its scheme
// alone with no prefix and no length, elides every query value, replaces blob
// digests and manifest references, and drops userinfo from every URL. There is
// no code path from raw response header or transport-error bytes to a line in
// these files, and response Content-Length retains only a fixed known/unknown
// marker.
func writeConformanceLog(t *testing.T, name, body string) {
	t.Helper()

	dir := os.Getenv(logDirEnv)
	if dir == "" {
		return
	}

	require.NoError(t, os.MkdirAll(dir, logDirMode))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), logMode))
}

// fileDigest is the hex sha256 of a file's contents, read in a stream so a
// comparison never holds a whole transfer in memory.
func fileDigest(t *testing.T, path string) string {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)

	sum := sha256.New()
	_, copyErr := io.Copy(sum, file)
	closeErr := file.Close()
	require.NoError(t, copyErr)
	require.NoError(t, closeErr)

	return hex.EncodeToString(sum.Sum(nil))
}

// runTag is the tag every reference this run writes to carries.
//
// It has to be unique per run, and the reason is not tidiness: a tag reused
// across runs points at whatever the last run left there, so a push that
// silently did nothing would still leave a pull with an artifact to fetch and
// every row would pass on a stale one. A re-run of the same workflow run
// carries the same run id, so the attempt number is part of it too.
func runTag() string {
	if id := os.Getenv(runIDEnv); id != "" {
		if attempt := os.Getenv(attemptEnv); attempt != "" {
			return "run-" + id + "-" + attempt
		}

		return "run-" + id
	}

	return "run-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}
