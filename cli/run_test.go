package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci"
)

// errPlain stands in for a failure the library does not classify.
var errPlain = errors.New("connection reset by peer")

// result is what one in-process run produced: the exit code and both streams,
// byte for byte as the real program would have written them.
type result struct {
	// code is the exit code run returned.
	code int
	// stdout is everything the run wrote to standard output.
	stdout string
	// stderr is everything the run wrote to standard error.
	stderr string
}

// runCLI runs one command line in process. Nothing here touches a registry: every
// case is answered by argument parsing or by the library's own validation.
func runCLI(t *testing.T, args ...string) result {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), env{args: args, stdout: &stdout, stderr: &stderr}, nil)

	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// interruptedBy returns an interrupt record that has already seen s, standing in
// for a signal handler that fired during a transfer.
func interruptedBy(s os.Signal) *interrupts {
	sig := &interrupts{}
	sig.record(s)

	return sig
}

// twoLayers wraps err the way a real failure arrives: the library's context
// around the transfer, and the transfer's context around whatever broke.
func twoLayers(err error) error {
	return fmt.Errorf("push model.bin to reg/repo:v1: %w", fmt.Errorf("part 3: %w", err))
}

// TestRunHelpGoesToStdout checks the one case where usage text is data: help that
// was asked for by name succeeds, and succeeds on standard output.
func TestRunHelpGoesToStdout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{name: "no arguments at all", args: nil, wants: []string{"usage:", "bigoci push", "bigoci pull"}},
		{name: "help", args: []string{"help"}, wants: []string{"usage:", "bigoci help"}},
		{name: "short flag", args: []string{"-h"}, wants: []string{"usage:"}},
		{name: "long flag", args: []string{"--help"}, wants: []string{"usage:"}},
		{
			name:  "help for push",
			args:  []string{"help", "push"},
			wants: []string{"usage: bigoci push", "-part-size", "-title", "-workers", "-debug", "-timeout"},
		},
		{
			name:  "help for pull",
			args:  []string{"help", "pull"},
			wants: []string{"usage: bigoci pull", "-workers", "-plain-http"},
		},
		{
			name:  "help asked for by flag on a command",
			args:  []string{"push", "-h"},
			wants: []string{"usage: bigoci push"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := runCLI(t, tt.args...)
			assert.Equal(t, exitOK, got.code)
			assert.Empty(t, got.stderr)
			for _, want := range tt.wants {
				assert.Contains(t, got.stdout, want)
			}
		})
	}
}

// TestRunHelpForPullOmitsPushFlags checks the claim pull's usage makes: a part size
// and a title are properties of the push that chose them, so pull does not offer
// to set either.
func TestRunHelpForPullOmitsPushFlags(t *testing.T) {
	t.Parallel()

	got := runCLI(t, "help", "pull")
	assert.Equal(t, exitOK, got.code)

	_, flags, found := strings.Cut(got.stdout, "flags:\n")
	require.True(t, found)
	assert.NotContains(t, flags, "-part-size")
	assert.NotContains(t, flags, "-title")
	assert.Contains(t, got.stdout, "There is no -part-size and no -title here")
}

// TestRunUsageErrors is the exit-2 surface. Every case asserts the same stream
// discipline: nothing on standard output, the complaint and a usage block on
// standard error.
func TestRunUsageErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{name: "unknown command", args: []string{"shove"}, wants: []string{`unknown command "shove"`}},
		{name: "unknown command for help", args: []string{"help", "shove"}, wants: []string{`unknown command "shove"`}},
		{name: "too much for help", args: []string{"help", "push", "pull"}, wants: []string{"at most one command"}},
		{name: "push with no operands", args: []string{"push"}, wants: []string{"exactly two operands", "got 0"}},
		{name: "push with one operand", args: []string{"push", "model.bin"}, wants: []string{"got 1"}},
		{
			name:  "push with three operands",
			args:  []string{"push", "model.bin", "reg/repo:v1", "extra"},
			wants: []string{"got 3"},
		},
		{name: "pull with one operand", args: []string{"pull", "reg/repo:v1"}, wants: []string{"got 1"}},
		{name: "empty operand", args: []string{"push", "", "reg/repo:v1"}, wants: []string{"operand 1 is empty"}},
		{
			name:  "flag after the operands",
			args:  []string{"push", "model.bin", "reg/repo:v1", "-debug"},
			wants: []string{"flags must come before the operands", `move "-debug" before "model.bin"`},
		},
		{
			name:  "flag after the operands on pull",
			args:  []string{"pull", "reg/repo:v1", "out.bin", "-workers", "8"},
			wants: []string{`move "-workers" before "reg/repo:v1"`},
		},
		{
			name:  "part size in decimal si units",
			args:  []string{"push", "-part-size", "4MB", "model.bin", "reg/repo:v1"},
			wants: []string{"decimal SI units", "4MiB"},
		},
		{
			name:  "part size on a pull",
			args:  []string{"pull", "-part-size", "4MiB", "reg/repo:v1", "out.bin"},
			wants: []string{"not defined", "part-size"},
		},
		{
			name:  "title on a pull",
			args:  []string{"pull", "-title", "model.bin", "reg/repo:v1", "out.bin"},
			wants: []string{"not defined", "title"},
		},
		{
			name:  "negative timeout on a push",
			args:  []string{"push", "-timeout", "-1s", "model.bin", "reg/repo:v1"},
			wants: []string{"push: -timeout must not be negative, got -1s"},
		},
		{
			name:  "negative timeout on a pull",
			args:  []string{"pull", "-timeout", "-1s", "reg/repo:v1", "out.bin"},
			wants: []string{"pull: -timeout must not be negative, got -1s"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := runCLI(t, tt.args...)
			assert.Equal(t, exitUsage, got.code)
			assert.Empty(t, got.stdout)
			assert.Contains(t, got.stderr, "usage:", "a usage error must print the usage block")
			for _, want := range tt.wants {
				assert.Contains(t, got.stderr, want)
			}
		})
	}
}

// TestRunPullRejectsDirectoryDestination checks the guard that saves a caller from
// a confusing failure: the library would write a partial file beside the directory
// and then fail to rename onto it.
func TestRunPullRejectsDirectoryDestination(t *testing.T) {
	t.Parallel()

	got := runCLI(t, "pull", "reg/repo:v1", t.TempDir())
	assert.Equal(t, exitUsage, got.code)
	assert.Empty(t, got.stdout)
	assert.Contains(t, got.stderr, "dest must be a file path, not a directory")
}

// TestRunPullAcceptsFileDestination checks the same guard does not fire on a path
// inside a directory, which is what a real pull is given.
func TestRunPullAcceptsFileDestination(t *testing.T) {
	t.Parallel()

	require.NoError(t, destMustBeFile(filepath.Join(t.TempDir(), "out.bin")))
}

// TestRunFlagsReachTheLibrary proves the flags are wired to the library rather
// than to a copy of its rules: an impossible worker count is refused by the
// library's own validation, before anything is opened or dialled.
func TestRunFlagsReachTheLibrary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "worker count on a push",
			args: []string{"push", "-workers", "-1", "model.bin", "reg/repo:v1"},
			want: "worker count must be positive",
		},
		{
			name: "worker count on a pull",
			args: []string{"pull", "-workers", "0", "reg/repo:v1", "out.bin"},
			want: "worker count must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := runCLI(t, tt.args...)
			assert.Equal(t, exitFailure, got.code)
			assert.Empty(t, got.stdout)
			assert.Contains(t, got.stderr, tt.want)
			assert.Contains(t, got.stderr, "no sentinel matched (exit 1)")
		})
	}
}

// TestRunMalformedReferenceIsAPlainFailure records a judgment call: the CLI never
// parses a reference, so a malformed one is whatever the library says it is, and
// today no sentinel covers it.
func TestRunMalformedReferenceIsAPlainFailure(t *testing.T) {
	t.Parallel()

	got := runCLI(t, "push", "model.bin", "not a reference")
	assert.Equal(t, exitFailure, got.code)
	assert.Empty(t, got.stdout)
	assert.Contains(t, got.stderr, "no sentinel matched (exit 1)")
}

// parsedPush parses one push command line and returns the flags it left behind
// with the names it actually set.
//
// Each call builds its own flag set, because a set that has already parsed once
// unions what a second parse visits and there is nothing in the standard
// library's contract that says it should not.
func parsedPush(t *testing.T, args ...string) (*pushFlags, map[string]bool) {
	t.Helper()

	f := &pushFlags{}
	fs := newFlagSet(cmdPush)
	f.register(fs)
	require.NoError(t, fs.Parse(args))

	return f, setFlagNames(fs)
}

// parsedPull parses one pull command line and returns the shared flags it left
// behind with the names it actually set.
func parsedPull(t *testing.T, args ...string) (commonFlags, map[string]bool) {
	t.Helper()

	var c commonFlags
	fs := newFlagSet(cmdPull)
	c.register(fs)
	require.NoError(t, fs.Parse(args))

	return c, setFlagNames(fs)
}

// TestPushOptionAssembly is the unset-means-absent seam. A flag left alone must
// contribute no option at all, so the library's own default applies; a flag set to
// its zero value must contribute one.
//
// The whole option slice is asserted rather than its length, because a length
// would pass just as happily on the wrong option carrying the wrong value.
func TestPushOptionAssembly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []bigoci.PushOption
	}{
		{name: "nothing set", args: []string{"model.bin", "reg/repo:v1"}, want: nil},
		{
			name: "title cleared on purpose",
			args: []string{"-title", "", "model.bin", "reg/repo:v1"},
			want: []bigoci.PushOption{bigoci.WithTitle("")},
		},
		{
			name: "title given",
			args: []string{"-title", "weights", "model.bin", "reg/repo:v1"},
			want: []bigoci.PushOption{bigoci.WithTitle("weights")},
		},
		{
			name: "part size alone",
			args: []string{"-part-size", "4MiB", "model.bin", "reg/repo:v1"},
			want: []bigoci.PushOption{bigoci.WithPartSize(4 * mib)},
		},
		{
			name: "workers alone",
			args: []string{"-workers", "8", "model.bin", "reg/repo:v1"},
			want: []bigoci.PushOption{bigoci.WithWorkers(8)},
		},
		{
			name: "every push flag",
			args: []string{"-part-size", "4MiB", "-title", "w", "-workers", "8", "model.bin", "reg/repo:v1"},
			want: []bigoci.PushOption{
				bigoci.WithPartSize(4 * mib), bigoci.WithTitle("w"), bigoci.WithWorkers(8),
			},
		},
		{
			name: "flags that are not push options",
			args: []string{"-plain-http", "-debug", "model.bin", "reg/repo:v1"},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, set := parsedPush(t, tt.args...)
			assert.Equal(t, tt.want, f.options(set))
		})
	}
}

// TestPullOptionAssembly is the same seam on the other command, where the worker
// count is the only option there is to assemble.
func TestPullOptionAssembly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []bigoci.PullOption
	}{
		{name: "nothing set", args: []string{"reg/repo:v1", "out.bin"}, want: nil},
		{
			name: "workers given",
			args: []string{"-workers", "8", "reg/repo:v1", "out.bin"},
			want: []bigoci.PullOption{bigoci.WithWorkers(8)},
		},
		{
			name: "flags that are not pull options",
			args: []string{"-plain-http", "-debug", "reg/repo:v1", "out.bin"},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, set := parsedPull(t, tt.args...)
			assert.Equal(t, tt.want, pullOptions(c, set))
		})
	}
}

// TestEffectiveValuesFallBackToTheLibrary checks that the preflight line reports
// the run rather than the command line: where a flag was not set, the value it
// names is the library's own.
func TestEffectiveValuesFallBackToTheLibrary(t *testing.T) {
	t.Parallel()

	unset, unsetNames := parsedPush(t, "model.bin", "reg/repo:v1")
	assert.Equal(t, bigoci.DefaultPartSize, unset.effectivePartSize(unsetNames))
	assert.Equal(t, bigoci.DefaultWorkers, unset.common.effectiveWorkers(unsetNames))

	given, givenNames := parsedPush(t, "-part-size", "4MiB", "-workers", "9", "model.bin", "reg/repo:v1")
	assert.Equal(t, bigoci.PartSize(4*mib), given.effectivePartSize(givenNames))
	assert.Equal(t, 9, given.common.effectiveWorkers(givenNames))
}

// TestPushPreflightLine checks the line that records what a push is about to do,
// including the case where the file cannot be measured and the line is left out
// rather than guessed at.
func TestPushPreflightLine(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "model.bin")
	require.NoError(t, os.WriteFile(src, bytes.Repeat([]byte{'x'}, 2048), 0o600))

	f, set := parsedPush(t, "-part-size", "1KiB", "-plain-http", src, "reg/repo:v1")

	line := f.preflight(set, src, "reg/repo:v1")
	assert.Contains(t, line, "bigoci: push "+src+" (2048 bytes) -> reg/repo:v1 (")
	assert.Contains(t, line, "part-size=1KiB")
	assert.Contains(t, line, "workers=4")
	assert.Contains(t, line, "plain-http")

	assert.Empty(t, f.preflight(set, filepath.Join(t.TempDir(), "absent"), "reg/repo:v1"))
}

// TestReportErrorExitCodes is the exit-code table. Each sentinel is wrapped two
// layers deep, because that is how a real failure arrives and the point of the
// table is that depth does not matter.
//
// The signal cases also assert what the second line does not say: a recorded
// signal outranks the error's shape, so the sentinel table never gets to speak
// about a run that was told to stop.
func TestReportErrorExitCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		sig      *interrupts
		code     int
		wants    []string
		notWants []string
	}{
		{
			name:  "not found",
			err:   twoLayers(bigoci.ErrNotFound),
			code:  exitNotFound,
			wants: []string{"matched sentinel bigoci.ErrNotFound (exit 3)"},
		},
		{
			name:  "not a bigoci artifact",
			err:   twoLayers(bigoci.ErrNotBigociArtifact),
			code:  exitNotBigociArtifact,
			wants: []string{"matched sentinel bigoci.ErrNotBigociArtifact (exit 4)"},
		},
		{
			name:  "digest mismatch",
			err:   twoLayers(bigoci.ErrDigestMismatch),
			code:  exitDigestMismatch,
			wants: []string{"matched sentinel bigoci.ErrDigestMismatch (exit 5)"},
		},
		{
			name:  "part too large",
			err:   twoLayers(bigoci.ErrPartTooLarge),
			code:  exitPartTooLarge,
			wants: []string{"matched sentinel bigoci.ErrPartTooLarge (exit 7)"},
		},
		{
			name:  "nothing the library classifies",
			err:   twoLayers(errPlain),
			code:  exitFailure,
			wants: []string{"no sentinel matched (exit 1)"},
		},
		{
			name:     "interrupted",
			err:      twoLayers(context.Canceled),
			sig:      interruptedBy(os.Interrupt),
			code:     exitInterrupted,
			wants:    []string{"bigoci: interrupted by SIGINT (exit 130)\n"},
			notWants: []string{"sentinel"},
		},
		{
			name:     "terminated",
			err:      twoLayers(context.Canceled),
			sig:      interruptedBy(syscall.SIGTERM),
			code:     exitTerminated,
			wants:    []string{"bigoci: terminated by SIGTERM (exit 143)\n"},
			notWants: []string{"sentinel"},
		},
		{
			name:     "interrupted, and the error was never a cancellation",
			err:      twoLayers(errors.New("file already closed")),
			sig:      interruptedBy(syscall.SIGINT),
			code:     exitInterrupted,
			wants:    []string{"bigoci: interrupted by SIGINT (exit 130)\n"},
			notWants: []string{"sentinel"},
		},
		{
			name:     "interrupted, and the error was one the library classifies",
			err:      twoLayers(bigoci.ErrNotFound),
			sig:      interruptedBy(syscall.SIGINT),
			code:     exitInterrupted,
			wants:    []string{"bigoci: interrupted by SIGINT (exit 130)\n"},
			notWants: []string{"sentinel"},
		},
		{
			name:  "cancelled with no signal behind it",
			err:   twoLayers(context.Canceled),
			code:  exitFailure,
			wants: []string{"no sentinel matched (exit 1)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := reportError(env{stdout: &stdout, stderr: &stderr}, tt.err, tt.sig)

			assert.Equal(t, tt.code, code)
			assert.Empty(t, stdout.String())
			assert.Contains(t, stderr.String(), "bigoci: "+tt.err.Error()+"\n")
			for _, want := range tt.wants {
				assert.Contains(t, stderr.String(), want)
			}
			for _, notWant := range tt.notWants {
				assert.NotContains(t, stderr.String(), notWant)
			}
		})
	}
}

// TestWithDeadline checks that a deadline the caller asked for is reported as one,
// and that the failure underneath still answers to [errors.Is].
func TestWithDeadline(t *testing.T) {
	t.Parallel()

	err := withDeadline(t.Context(), time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()

		return fmt.Errorf("push model.bin to reg/repo:v1: %w", ctx.Err())
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out after 1ms")
	assert.Contains(t, err.Error(), "push model.bin to reg/repo:v1")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestWithDeadlineUnsetAddsNoBound checks that an unset -timeout adds no deadline
// at all, rather than a very long one.
func TestWithDeadlineUnsetAddsNoBound(t *testing.T) {
	t.Parallel()

	require.NoError(t, withDeadline(t.Context(), 0, func(ctx context.Context) error {
		_, hasDeadline := ctx.Deadline()
		assert.False(t, hasDeadline)

		return nil
	}))
}

// TestValidateTimeout checks the one shared flag value no transfer can run with,
// and the explicit zero beside it that means "no limit" rather than a mistake.
//
// It calls validate rather than a whole command line because an accepted zero
// has to be checked without reaching a registry: the run that follows it fails
// for its own reasons, which say nothing about this guard.
func TestValidateTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout time.Duration
		set     bool
		wantErr string
	}{
		{name: "left alone", timeout: 0, set: false},
		{name: "explicitly no limit", timeout: 0, set: true},
		{name: "a real deadline", timeout: 30 * time.Second, set: true},
		{
			name:    "negative",
			timeout: -time.Second,
			set:     true,
			wantErr: "push: -timeout must not be negative, got -1s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			set := map[string]bool{}
			if tt.set {
				set[flagTimeout] = true
			}

			c := commonFlags{timeout: tt.timeout}
			err := c.validate(set, cmdPush, pushUsage())
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.EqualError(t, err, tt.wantErr)

			var usage *usageError
			require.ErrorAs(t, err, &usage, "a refused timeout must exit 2 and print the usage block")
			assert.Contains(t, usage.usage, "usage: bigoci push")
		})
	}
}

// TestMisplacedFlag checks the guard that refuses to silently drop a flag written
// after the operands.
func TestMisplacedFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		operands []string
		want     int
	}{
		{name: "none", operands: []string{"model.bin", "reg/repo:v1"}, want: -1},
		{name: "trailing", operands: []string{"model.bin", "reg/repo:v1", "-debug"}, want: 2},
		{name: "leading", operands: []string{"-debug", "model.bin"}, want: 0},
		{name: "empty", operands: nil, want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, misplacedFlag(tt.operands))
		})
	}
}

// TestTerminatorDisarmsTheMisplacedFlagGuard checks the escape the guard's own
// complaint teaches: after "--", an operand that begins with a dash is exactly
// what the caller meant, so the run proceeds and fails on the operand instead.
//
// It stays hermetic because a push opens the file before it dials anything, so a
// file that is not there ends the run with no request sent.
func TestTerminatorDisarmsTheMisplacedFlagGuard(t *testing.T) {
	t.Parallel()

	got := runCLI(t, "push", "--", "-dash.bin", "reg.example.com/team/model:v1")

	assert.Equal(t, exitFailure, got.code)
	assert.Empty(t, got.stdout)
	assert.Contains(t, got.stderr, "bigoci: open -dash.bin: no such file or directory")
	assert.Contains(t, got.stderr, "no sentinel matched (exit 1)")
	assert.NotContains(t, got.stderr, "flags must come")
	assert.NotContains(t, got.stderr, "usage:")
}

// TestMisplacedFlagErrorTeachesTheTerminator checks that the complaint about an
// operand-shaped first operand names the escape rather than only refusing.
func TestMisplacedFlagErrorTeachesTheTerminator(t *testing.T) {
	t.Parallel()

	c := command{flags: newFlagSet(cmdPush), name: cmdPush, syntax: "<file> <ref>", usage: pushUsage()}

	leading := c.misplacedFlagError([]string{"-dash.bin", "reg/repo:v1"}, 0)
	assert.Contains(t, leading.Error(), `"-dash.bin" is not one of them (write it after -- if it is an operand)`)

	trailing := c.misplacedFlagError([]string{"model.bin", "reg/repo:v1", "-debug"}, 2)
	assert.Contains(t, trailing.Error(), `move "-debug" before "model.bin"`)
}

// TestTopUsageTeachesTheTerminator checks that the rule and its escape are stated
// together, so a caller reading the top-level usage learns both at once.
func TestTopUsageTeachesTheTerminator(t *testing.T) {
	t.Parallel()

	assert.Contains(t, topUsage(), `Flags must come before the operands, and "--" ends the flags`)
}
