package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/componere/bigoci"
)

// runPull parses pull's command line and runs the pull it describes.
//
// A pull is told less than a push because it needs less: the part size and the
// file name travel in the manifest, so the worker count is the only thing left
// to say about the transfer.
func runPull(ctx context.Context, e env, args []string) error {
	var c commonFlags
	fs := newFlagSet(cmdPull)
	c.register(fs)

	operands, err := command{flags: fs, name: cmdPull, syntax: "<ref> <dest>", usage: pullUsage()}.parse(e, args)
	if err != nil {
		return err
	}
	ref, dest := operands[0], operands[1]

	set := setFlagNames(fs)
	if validateErr := c.validate(set, cmdPull, pullUsage()); validateErr != nil {
		return validateErr
	}
	if destErr := destMustBeFile(dest); destErr != nil {
		return destErr
	}

	// The tap and the progress renderer are the only two writers this program
	// ever runs at once, so they share one guarded stream and everything else
	// keeps writing to stderr directly.
	lines := sharedStderr(e.stderr, c.progress)

	client, probe, err := newClient(c, lines)
	if err != nil {
		return err
	}
	fmt.Fprintf(
		e.stderr, "bigoci: pull %s -> %s (%s)\n",
		terminalSafeLine(ref), terminalSafeLine(dest), c.settings(set),
	)

	watch := startProgress(e, lines, c.progress)

	// The callback is added here rather than in pullOptions, which stays a
	// pure function of the command line: a watcher is something this run
	// built, not a value a flag carried.
	opts := pullOptions(c, set)
	if watch != nil {
		opts = append(opts, bigoci.WithProgress(watch.record))
	}

	started := time.Now()
	err = withDeadline(ctx, c.timeout, func(ctx context.Context) error {
		return client.Pull(ctx, bigoci.Reference(ref), bigoci.ToFile(dest), opts...)
	})

	// The final progress line comes first, so the last thing said about the
	// transfer while it ran sits above everything said about how it ended.
	watch.stop()

	if probe != nil {
		probe.writeSummary()
	}
	if err != nil {
		return err
	}

	writePulled(e, time.Since(started).Round(resultPrecision))

	return nil
}

// pullUsage is pull's usage text: what the command does, then the flags it
// accepts with their real defaults.
func pullUsage() string {
	var c commonFlags
	fs := newFlagSet(cmdPull)
	c.register(fs)

	return usageBlock(`usage: bigoci pull [flags] <ref> <dest>

Read the manifest <ref> names, fetch its parts in parallel, and write them into
the file <dest>. Nothing goes to stdout. <dest> is published with one rename
once every part has verified, so it is never seen half written.

There is no -part-size and no -title here: both describe how a push chose to
store the file, and both travel in the manifest.

flags:
`, fs)
}

// pullOptions returns the library options a pull command line asked for, which
// is the worker count when -workers was set and nothing at all when it was not.
func pullOptions(c commonFlags, set map[string]bool) []bigoci.PullOption {
	var opts []bigoci.PullOption
	if set[flagWorkers] {
		opts = append(opts, bigoci.WithWorkers(c.workers))
	}

	return opts
}

// destMustBeFile rejects a destination that already exists as a directory.
//
// The library would write "<dest>.bigoci-partial" beside it and then fail the
// rename onto a directory, which is a confusing way to learn what went wrong.
// This is recorded as feedback on the library's API rather than worked around
// any further here.
func destMustBeFile(dest string) error {
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		return usageErrorf(pullUsage(), "pull: dest must be a file path, not a directory: %s", dest)
	}

	return nil
}

// writePulled reports a finished pull without repeating its registry-selected
// file size. A registry can choose an all-decimal bearer and serve exactly that
// many bytes, so even a size read back from the published file is not safe to
// copy into CLI output.
func writePulled(e env, took time.Duration) {
	fmt.Fprintf(e.stderr, "bigoci: pulled in %s\n", took)
}
