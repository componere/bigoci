package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/componere/bigoci"
)

// runPush parses push's command line and runs the push it describes.
//
// Nothing about the file is checked here. A missing or unreadable file is the
// library's report to make, and a reference is passed through exactly as typed:
// the CLI knows no reference grammar and never wants to disagree with the one
// the library uses.
func runPush(ctx context.Context, e env, args []string) error {
	var f pushFlags
	fs := newFlagSet(cmdPush)
	f.register(fs)

	operands, err := command{flags: fs, name: cmdPush, syntax: "<file> <ref>", usage: pushUsage()}.parse(e, args)
	if err != nil {
		return err
	}
	src, ref := operands[0], operands[1]

	set := setFlagNames(fs)
	if validateErr := f.common.validate(set, cmdPush, pushUsage()); validateErr != nil {
		return validateErr
	}

	// The tap and the progress renderer are the only two writers this program
	// ever runs at once, so they share one guarded stream and everything else
	// keeps writing to stderr directly.
	lines := sharedStderr(e.stderr, f.common.progress)

	client, probe, err := newClient(f.common, lines)
	if err != nil {
		return err
	}
	fmt.Fprint(e.stderr, f.preflight(set, src, ref))

	watch := startProgress(e, lines, f.common.progress)

	// The callback is added here rather than in options, which stays a pure
	// function of the command line: a watcher is something this run built, not
	// a value a flag carried.
	opts := f.options(set)
	if watch != nil {
		opts = append(opts, bigoci.WithProgress(watch.record))
	}

	started := time.Now()
	var digest string
	err = withDeadline(ctx, f.common.timeout, func(ctx context.Context) error {
		desc, pushErr := client.Push(ctx, bigoci.Reference(ref), bigoci.FromFile(src), opts...)
		digest = desc.Digest.String()

		return pushErr
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

	fmt.Fprintln(e.stdout, digest)
	fmt.Fprintf(e.stderr, "bigoci: pushed %s in %s\n", digest, time.Since(started).Round(resultPrecision))

	return nil
}

// pushUsage is push's usage text: what the command does, then the flags it
// accepts with their real defaults.
func pushUsage() string {
	var f pushFlags
	fs := newFlagSet(cmdPush)
	f.register(fs)

	return usageBlock(`usage: bigoci push [flags] <file> <ref>

Split <file> into parts, upload the parts <ref>'s repository does not already
hold, and write the manifest that lists them. The digest of that manifest goes
to stdout on a line of its own; everything else goes to stderr.

flags:
`, fs)
}

// pushFlags holds what a push command line asked for: the flags both commands
// share, and the two that describe how the file is stored.
type pushFlags struct {
	// common are the flags both commands declare.
	common commonFlags
	// partSize is the size the file is split at.
	partSize partSizeValue
	// title is the file name the manifest records.
	title string
}

// register declares push's flags on fs.
//
// Both defaults here are zero values rather than the library's, so an unset
// flag stays visibly unset. The help text names the real default, read from the
// library at runtime: re-measuring one changes what the CLI says and does with
// no edit here.
func (p *pushFlags) register(fs *flag.FlagSet) {
	p.common.register(fs)
	fs.Var(&p.partSize, flagPartSize, fmt.Sprintf(
		"`size` of each part, as a byte count or 4MiB, 512K, 1G (unset: the library default, %s)",
		formatSize(bigoci.DefaultPartSize),
	))
	fs.StringVar(&p.title, flagTitle, "",
		`file name the manifest records (unset: the base name of <file>; "" records none)`)
}

// options returns the library options the command line asked for, one per flag
// it actually set.
//
// A flag left alone contributes nothing, so the library's own default applies.
// That is the whole reason -title set to the empty string differs from no
// -title: the first clears the annotation, the second lets the library name the
// artifact after the file.
func (p *pushFlags) options(set map[string]bool) []bigoci.PushOption {
	var opts []bigoci.PushOption
	if set[flagPartSize] {
		opts = append(opts, bigoci.WithPartSize(bigoci.PartSize(p.partSize)))
	}
	if set[flagTitle] {
		opts = append(opts, bigoci.WithTitle(p.title))
	}
	if set[flagWorkers] {
		opts = append(opts, bigoci.WithWorkers(p.common.workers))
	}

	return opts
}

// effectivePartSize is the size the push will really split at: the flag where it
// was set, the library's own default where it was not.
func (p *pushFlags) effectivePartSize(set map[string]bool) bigoci.PartSize {
	if set[flagPartSize] {
		return bigoci.PartSize(p.partSize)
	}

	return bigoci.DefaultPartSize
}

// preflight renders the line that records what the push is about to do, with
// the values it will really run with rather than the ones that were typed.
//
// The byte count comes from [os.Stat]. When that fails the whole line is left
// out: the library is the one that reports an unreadable file, and a preflight
// line is no place to guess at why. Both operands visibly escape non-graphic
// bytes so a file name or reference cannot create another terminal record.
func (p *pushFlags) preflight(set map[string]bool, src, ref string) string {
	info, err := os.Stat(src)
	if err != nil {
		return ""
	}

	return fmt.Sprintf(
		"bigoci: push %s (%d bytes) -> %s (part-size=%s, %s)\n",
		terminalSafeLine(src), info.Size(), terminalSafeLine(ref),
		formatSize(p.effectivePartSize(set)), p.common.settings(set),
	)
}
