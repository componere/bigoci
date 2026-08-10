package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// Process exit codes. Two is what the flag package's own usage failures
// produce, so the harness keeps the same meaning for its own.
const (
	// exitOK means every requested measurement succeeded.
	exitOK = 0
	// exitFailure means the run aborted, or finished with failed rows.
	exitFailure = 1
	// exitUsage means the command line or spec was unusable.
	exitUsage = 2
)

// Subcommand names, shared by the dispatcher and the flag sets.
const (
	// cmdRun is the measurement subcommand.
	cmdRun = "run"
	// cmdHelp is the help subcommand.
	cmdHelp = "help"
	// cmdSummarize is the rendering subcommand.
	cmdSummarize = "summarize"
)

// run dispatches the subcommand and returns the process exit code. It is
// main with the process edges cut off, so tests can call it with buffers.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)

		return exitUsage
	}

	switch args[0] {
	case cmdRun:
		return runBench(ctx, args[1:], stderr)
	case cmdSummarize:
		return runSummarize(args[1:], stdout, stderr)
	case cmdHelp, "-h", "-help", "--help":
		usage(stdout)

		return exitOK
	default:
		fmt.Fprintf(stderr, "bench: unknown command %q\n", args[0])
		usage(stderr)

		return exitUsage
	}
}

// usage writes the command summary.
func usage(w io.Writer) {
	fmt.Fprint(w, `bench measures bigoci transfer throughput.

Commands:
  bench run -spec <spec.json> -out <results.jsonl> [-resume] [-run-id id] [-endpoint name=host:port]
  bench summarize -in <results.jsonl>

run walks the matrix a spec describes and appends one JSONL row per timed
scenario. summarize renders recorded rows as Markdown tables.
`)
}

// endpointFlag collects repeated -endpoint name=host:port overrides.
type endpointFlag map[string]string

// String renders the collected overrides; empty when none were set, which
// keeps the flag package from printing a default.
func (f endpointFlag) String() string {
	if len(f) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(f))
	for name, endpoint := range f {
		pairs = append(pairs, name+"="+endpoint)
	}

	return strings.Join(pairs, ",")
}

// Set parses one -endpoint argument.
func (f endpointFlag) Set(text string) error {
	name, endpoint, found := strings.Cut(text, "=")
	if !found || name == "" || endpoint == "" {
		return errors.New("write -endpoint name=host:port, naming a target from the spec")
	}
	f[name] = endpoint

	return nil
}

// runBench executes the run subcommand.
func runBench(ctx context.Context, args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("bench run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	specPath := flags.String("spec", "", "path to the run-spec JSON file (required)")
	outPath := flags.String("out", "", "path of the JSONL results file to append to (required)")
	resume := flags.Bool("resume", false, "skip measurements already recorded in the output file")
	runID := flags.String("run-id", "", "override the spec run_id for this measurement session")
	endpoints := endpointFlag{}
	flags.Var(endpoints, "endpoint", "override a target's endpoint as name=host:port (repeatable)")

	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if *specPath == "" || *outPath == "" {
		fmt.Fprintln(stderr, "bench run: -spec and -out are both required")

		return exitUsage
	}

	spec, err := loadSpec(*specPath)
	if err != nil {
		fmt.Fprintf(stderr, "bench run: %v\n", err)

		return exitUsage
	}
	if endpointErr := applyEndpoints(spec, endpoints); endpointErr != nil {
		fmt.Fprintf(stderr, "bench run: %v\n", endpointErr)

		return exitUsage
	}
	if runIDErr := applyRunID(spec, *runID); runIDErr != nil {
		fmt.Fprintf(stderr, "bench run: %v\n", runIDErr)

		return exitUsage
	}

	failed, err := runMatrix(ctx, spec, *outPath, *resume, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "bench run: %v\n", err)

		return exitFailure
	}
	if failed > 0 {
		fmt.Fprintf(stderr, "bench run: %d measurement(s) failed; see the error rows in %s\n", failed, *outPath)

		return exitFailure
	}

	return exitOK
}

// applyRunID substitutes a nonempty command-line run ID after validating it
// with the same repository-path grammar as the spec.
func applyRunID(spec *Spec, runID string) error {
	if runID == "" {
		return nil
	}
	if !validPathSegment(runID) {
		return fmt.Errorf("-run-id %q: use lowercase letters, digits, dot, dash, and underscore", runID)
	}
	spec.RunID = runID

	return nil
}

// applyEndpoints substitutes the -endpoint overrides into the spec,
// refusing names the spec does not define — a typo must not send a stage
// against the placeholder endpoint.
func applyEndpoints(spec *Spec, endpoints endpointFlag) error {
	for name, endpoint := range endpoints {
		found := false
		for i := range spec.Targets {
			if spec.Targets[i].Name == name {
				spec.Targets[i].Endpoint = endpoint
				found = true
			}
		}
		if !found {
			return fmt.Errorf("-endpoint %s: the spec defines no target by that name", name)
		}
	}

	return nil
}

// runMatrix walks every cell and iteration of the spec, appending rows to
// the output file, and returns how many measurements failed.
//
// A failed measurement is recorded and skipped past — a paid benchmark box
// should never sit idle because one cell went bad. Only a context
// cancellation or a failure to record results stops the walk.
func runMatrix(ctx context.Context, spec *Spec, outPath string, resume bool, stderr io.Writer) (int, error) {
	commit := buildCommit()
	cohortID, err := newCohortID(spec, commit)
	if err != nil {
		return 0, err
	}
	skip, err := prepareOutput(outPath, resume, spec.RunID, cohortID)
	if err != nil {
		return 0, err
	}
	attemptID, err := newAttemptID()
	if err != nil {
		return 0, err
	}

	if spec.FixtureDir != "" {
		if mkdirErr := os.MkdirAll(spec.FixtureDir, 0o750); mkdirErr != nil {
			return 0, fmt.Errorf("create fixture dir: %w", mkdirErr)
		}
	}

	writer, err := newRowWriter(outPath)
	if err != nil {
		return 0, err
	}
	defer writer.close()

	cells := expand(spec)
	walk := &matrixWalk{
		spec:      spec,
		attemptID: attemptID,
		cohortID:  cohortID,
		commit:    commit,
		skip:      skip,
		writer:    writer,
		log: func(format string, args ...any) {
			fmt.Fprintf(stderr, format+"\n", args...)
		},
	}

	for i, c := range cells {
		walk.log("cell %d/%d %s", i+1, len(cells), c.id)
		if err := walk.cell(ctx, c); err != nil {
			return walk.failed, err
		}
	}

	return walk.failed, nil
}

// matrixWalk carries the state one runMatrix invocation threads through
// every cell: the spec, the resume set, the writer, and the running failure
// count.
type matrixWalk struct {
	// spec is the run's spec.
	spec *Spec
	// attemptID isolates this process attempt's fixtures and repositories.
	attemptID string
	// cohortID binds every row to this effective spec and harness build.
	cohortID string
	// commit is the human-readable harness revision stored beside the cohort.
	commit string
	// skip is the resume set of already-measured rows.
	skip map[string]bool
	// writer records finished rows.
	writer *rowWriter
	// log writes one progress line.
	log func(format string, args ...any)
	// failed counts the error rows recorded so far.
	failed int
}

// cell runs every iteration of one cell.
func (w *matrixWalk) cell(ctx context.Context, c cell) error {
	client, counter, err := newTargetClient(c.target)
	if err != nil {
		return err
	}

	for number := range w.spec.Iterations {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("run stopped: %w", err)
		}

		it := &iteration{
			spec:      w.spec,
			cell:      c,
			number:    number,
			attemptID: w.attemptID,
			cohortID:  w.cohortID,
			commit:    w.commit,
			client:    client,
			counter:   counter,
			skip:      w.skip,
			emit: func(r row) error {
				if r.Error != "" {
					w.failed++
				}

				return w.writer.write(r)
			},
			log: w.log,
		}
		if err := it.run(ctx); err != nil {
			return err
		}
	}

	return nil
}
