package bigoci_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci"
)

// How a helper child is told what to move. Everything the child needs arrives
// in these variables and nothing else does: the parent hands over an explicit
// environment, so a child never inherits the -test flags, the coverage
// directory, or anything else the run that started it was configured with.
const (
	// helperModeEnv selects the transfer the child runs, and is what makes a
	// test binary a helper at all. [TestMain] reads it before it looks at
	// anything else, so an ordinary run — where it is unset — is untouched.
	helperModeEnv = "BIGOCI_E2E_HELPER"
	// helperRefEnv is the reference the child transfers to or from, which is
	// normally a counting proxy's address rather than the registry's own.
	helperRefEnv = "BIGOCI_E2E_REF"
	// helperPathEnv is the local file the child reads or writes.
	helperPathEnv = "BIGOCI_E2E_PATH"
	// helperPartSizeEnv is the part size a child push splits at, in bytes. A
	// pull reads the part size out of the manifest and ignores it.
	helperPartSizeEnv = "BIGOCI_E2E_PART_SIZE"
	// helperWorkersEnv is how many parts the child moves at once.
	helperWorkersEnv = "BIGOCI_E2E_WORKERS"
	// helperCatchEnv, when set, makes the child handle an interrupt instead of
	// dying of it. It is what separates the graceful row from the kill rows.
	helperCatchEnv = "BIGOCI_E2E_CATCH_SIGINT"
	// helperPull names the mode that runs one pull.
	helperPull = "pull"
	// helperPush names the mode that runs one push.
	helperPush = "push"
)

// How a helper child ends. The parent reads these back off the exit status,
// which is the only channel a killed process leaves it.
const (
	// helperOK is the status of a child whose transfer finished.
	helperOK = 0
	// helperFailed is the status of a child whose transfer returned an error
	// of any other kind. The error itself goes to the child's stderr, which
	// the parent logs when the row fails.
	helperFailed = 1
	// helperInterrupted is the status of a child whose transfer ended because
	// its context was cancelled. It is the CLI's exit-130 contract, which is
	// what an interrupted transfer looks like from outside.
	helperInterrupted = 130
)

// helperWaitDelay is how long [os/exec.Cmd.Wait] keeps reading a killed child's
// output before it gives up on it, which is what stops a wedged child from
// hanging the row instead of failing it. It sits on no success path: a child
// that died takes its pipes with it.
const helperWaitDelay = 15 * time.Second

// credentialEnv returns the environment variables a credential lookup reads:
// the Docker configuration directory, and nothing else.
//
// DOCKER_CONFIG alone is a complete gate, because the library consults a home
// directory only when that variable names nothing — and [runTests] always
// makes it name an empty directory, which every helper child carries too, so
// a transfer in this package resolves every registry to the anonymous
// credential unless the row that ran it planted one. HOME and USERPROFILE are
// deliberately left alone: testcontainers resolves rootless Docker sockets
// and its own image-pull credentials through them, and redirecting the home
// of the whole test binary would break the registries these tests stand up.
func credentialEnv() []string {
	return []string{"DOCKER_CONFIG"}
}

// TestMain runs one transfer and exits when the environment asks for a helper
// child, and the package's tests otherwise.
//
// The kill rows need a transfer in a process of its own, because the thing
// they are testing is what a pull or a push leaves behind when the process
// running it stops existing — and a test cannot SIGKILL itself and then go on
// to assert anything. Re-execing the test binary is the cheapest honest way to
// get one: the child is the same code, built the same way, talking to the same
// registry, and it never reaches [testing.M.Run], so it runs no test.
//
// The ordinary run also isolates the credential environment, which every row
// that authenticates depends on: the rows that mean to transfer anonymously
// have to be unable to find a real credential, and the rows that plant one
// have to be the only thing the lookup can see.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		os.Exit(runHelper(mode))
	}

	os.Exit(runTests(m))
}

// runTests points the credential environment at a directory that holds
// nothing, runs the package's tests, and returns the status the process should
// leave with.
//
// PATH and the home variables are left exactly as they are, unlike the CLI
// package's isolation: these tests start registries with testcontainers,
// which finds the docker command on PATH and the Docker endpoint and its own
// pull credentials through the home. Nothing here names a credential helper,
// and DOCKER_CONFIG being set means no bigoci lookup ever reaches a home.
//
// The status travels through a variable rather than straight into [os.Exit] so
// the temporary directory is still removed on the way out.
func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "bigoci-e2e-test")
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolate the credential environment: %v\n", err)

		return helperFailed
	}
	defer func() { _ = os.RemoveAll(dir) }()

	for _, name := range credentialEnv() {
		if err := os.Setenv(name, dir); err != nil {
			fmt.Fprintf(os.Stderr, "isolate %s: %v\n", name, err)

			return helperFailed
		}
	}

	return m.Run()
}

// runHelper runs the one transfer the environment describes and returns the
// status the child exits with.
//
// The interrupt handler is two lines and installed only when the row asks for
// it, which is the whole difference between the graceful row and the kill
// rows: with it the child cancels its transfer and exits on the interrupted
// path, without it the signal kills the process where it stands.
func runHelper(mode string) int {
	ctx := context.Background()

	if os.Getenv(helperCatchEnv) != "" {
		var stop context.CancelFunc

		ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
	}

	if err := helperTransfer(ctx, mode); err != nil {
		fmt.Fprintln(os.Stderr, err)

		// The context is what is asked, not the error. A transfer the library
		// stopped because its context ended reports the failure it was
		// retrying rather than the cancellation, on purpose, so the only thing
		// that can say why the process is ending is the context itself.
		if ctx.Err() != nil {
			return helperInterrupted
		}

		return helperFailed
	}

	return helperOK
}

// helperTransfer runs the child's single transfer with a client of the child's
// own: this process shares no transport, no connection pool, and no state with
// the parent that started it, which is the point of running it out of process.
func helperTransfer(ctx context.Context, mode string) error {
	client, err := bigoci.New(bigoci.WithPlainHTTP(), bigoci.WithHTTPClient(helperClient()))
	if err != nil {
		return err
	}

	ref := bigoci.Reference(os.Getenv(helperRefEnv))
	target := os.Getenv(helperPathEnv)

	workers, err := strconv.Atoi(os.Getenv(helperWorkersEnv))
	if err != nil {
		return fmt.Errorf("read %s: %w", helperWorkersEnv, err)
	}

	switch mode {
	case helperPull:
		return client.Pull(ctx, ref, bigoci.ToFile(target), bigoci.WithWorkers(workers))
	case helperPush:
		size, err := strconv.ParseInt(os.Getenv(helperPartSizeEnv), decimalBase, 64)
		if err != nil {
			return fmt.Errorf("read %s: %w", helperPartSizeEnv, err)
		}

		_, err = client.Push(
			ctx, ref, bigoci.FromFile(target),
			bigoci.WithPartSize(bigoci.PartSize(size)), bigoci.WithWorkers(workers),
		)

		return err
	default:
		return fmt.Errorf("unknown helper mode %q", mode)
	}
}

// helperClient builds the HTTP client the child's transfer runs on: a
// transport of this process's own, so nothing the parent did to its own
// connection pool can reach it.
func helperClient() *http.Client {
	shared, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{}
	}

	return &http.Client{Transport: shared.Clone()}
}

// helperSpec is one child transfer: which direction it runs, what it moves,
// and the knobs the row needs it to run with.
type helperSpec struct {
	// mode is [helperPull] or [helperPush].
	mode string
	// ref is the reference the child transfers to or from.
	ref bigoci.Reference
	// path is the local file the child reads or writes.
	path string
	// partSize is the size a child push splits at.
	partSize bigoci.PartSize
	// workers is how many parts the child moves at once. One makes the
	// pipeline sequential, which is what turns a single request into proof
	// about every part before it.
	workers int
	// catchInterrupt makes the child handle an interrupt by cancelling its
	// transfer rather than dying of it.
	catchInterrupt bool
}

// env returns the whole environment the child runs with. It is built rather
// than inherited so that nothing about the parent's test run — its flags, its
// coverage directory, its own helper variables — can reach the child.
//
// Two sets of variables are carried through anyway. The Go runtime's own knobs
// come first: a maintainer chasing a scheduling bug in a kill row reaches for
// GOMAXPROCS or GODEBUG, and an environment that silently dropped them would
// perturb only the parent while the process actually being killed ran
// untouched. The credential variables come second, and they are carried for
// the opposite reason: [runTests] pointed them at an empty directory, and a
// child that inherited none of them would look for a configuration under the
// home of whoever ran the tests.
func (s helperSpec) env() []string {
	env := []string{
		helperModeEnv + "=" + s.mode,
		helperRefEnv + "=" + string(s.ref),
		helperPathEnv + "=" + s.path,
		helperPartSizeEnv + "=" + strconv.FormatInt(int64(s.partSize), decimalBase),
		helperWorkersEnv + "=" + strconv.Itoa(s.workers),
	}

	knobs := []string{"GOMAXPROCS", "GODEBUG", "GOTRACEBACK", "GORACE"}
	for _, knob := range append(knobs, credentialEnv()...) {
		if value, ok := os.LookupEnv(knob); ok {
			env = append(env, knob+"="+value)
		}
	}

	if s.catchInterrupt {
		env = append(env, helperCatchEnv+"=1")
	}

	return env
}

// helper is one child process running one transfer, and the two moments the
// rest of the harness has to be able to wait for: the process existing, and
// the process being gone.
type helper struct {
	// cmd is the child.
	cmd *exec.Cmd
	// out collects everything the child wrote to either stream. It is read
	// only after [helper.wait] has returned, because until then the copy
	// goroutines os/exec runs are still writing into it.
	out *bytes.Buffer
	// started closes once [os/exec.Cmd.Start] has returned, which is what lets
	// the proxy ask for a process it can signal without racing the fork.
	started chan struct{}
	// exited closes once the child has been reaped, which is what lets the
	// proxy hold its traffic until the signal it sent has provably landed.
	exited chan struct{}
}

// newHelper prepares a child that will run the transfer spec describes.
//
// It deliberately does not start it. A row arms its proxy against the helper
// first, so the trigger cannot miss a request a child made before anything was
// watching, and the proxy blocks on [helper.process] rather than reading a
// field the fork might not have filled in yet.
func newHelper(t *testing.T, spec helperSpec) *helper {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err, "find the test binary to re-exec as a helper child")

	h := &helper{
		cmd:     exec.CommandContext(t.Context(), exe),
		out:     &bytes.Buffer{},
		started: make(chan struct{}),
		exited:  make(chan struct{}),
	}

	h.cmd.Env = spec.env()
	h.cmd.Stdout = h.out
	h.cmd.Stderr = h.out
	// The child leads a process group of its own, so a kill aimed at the group
	// reaches the transfer and nothing else — least of all the test process,
	// which shares the terminal with it.
	h.cmd.SysProcAttr = processGroupAttr()
	h.cmd.Cancel = func() error { return h.signal(syscall.SIGKILL) }
	h.cmd.WaitDelay = helperWaitDelay

	return h
}

// start runs the child and publishes it to whatever is waiting to signal it.
func (h *helper) start(t *testing.T) {
	t.Helper()

	require.NoError(t, h.cmd.Start(), "start the helper child")
	close(h.started)
}

// process returns the running child, waiting for the start it belongs to. The
// wait is what makes the proxy's trigger safe: it can only fire on a request
// the child made, and a child that made a request has been started.
func (h *helper) process() *os.Process {
	<-h.started

	return h.cmd.Process
}

// signal sends sig to the child's whole process group.
func (h *helper) signal(sig syscall.Signal) error {
	proc := h.process()
	if proc == nil {
		return errors.New("the helper child was never started")
	}

	return signalGroup(proc.Pid, sig)
}

// wait reaps the child and returns how it ended.
//
// Nothing may look at the destination directory before this returns: a process
// that has not been reaped may still have writes in flight, and an assertion
// made against a file the child is still holding would be reading a race
// rather than a result.
//
// The child's output is captured here and logged only when the row fails,
// which keeps a passing suite quiet and a failing one explicit about what the
// other process saw.
func (h *helper) wait(t *testing.T) *os.ProcessState {
	t.Helper()

	waitErr := h.cmd.Wait()
	close(h.exited)

	output := h.out.String()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the helper child ended with %v and wrote:\n%s", waitErr, output)
		}
	})

	require.NotNil(t, h.cmd.ProcessState, "the helper child left no exit status: %v", waitErr)

	return h.cmd.ProcessState
}

// requireKilled fails the row unless the child was killed where it stood.
//
// This is the assertion that makes a silent pass loud, and it asks whether the
// process was signalled rather than whether it merely finished. A child that
// completed its transfer before the signal arrived would leave a destination
// that is whole and a rerun that fetches nothing, and every outcome assertion
// after it would pass while proving the opposite of what the row claims.
func requireKilled(t *testing.T, state *os.ProcessState) {
	t.Helper()

	status, ok := state.Sys().(syscall.WaitStatus)
	require.True(t, ok, "the child's exit status is not a wait status")
	require.True(
		t, status.Signaled(),
		"the helper child exited on its own with status %d: the signal landed after the transfer was over, "+
			"so this row proved nothing",
		state.ExitCode(),
	)
	require.Equal(t, syscall.SIGKILL, status.Signal(), "the child died of the wrong signal")
}

// requireInterrupted fails the row unless the child ended on the path an
// interrupt it handled takes: its transfer's context cancelled, its process
// exiting of its own accord with the interrupted status.
func requireInterrupted(t *testing.T, state *os.ProcessState) {
	t.Helper()

	status, ok := state.Sys().(syscall.WaitStatus)
	require.True(t, ok, "the child's exit status is not a wait status")
	require.False(
		t, status.Signaled(),
		"the child died of %s instead of handling it, so this row tested the kill path and not the graceful one",
		status.Signal(),
	)
	require.Equal(
		t, helperInterrupted, state.ExitCode(),
		"the child must have ended because its transfer was cancelled, and nothing else",
	)
}
