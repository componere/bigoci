// Command bigoci pushes and pulls large files with the bigoci library.
//
// This is a reference command line interface: verification tooling, never
// published, never released, never versioned. It exists so a human can watch
// the library work against a real registry, and it is the instrument for the
// manual gates of the library's implementation phases. Nothing in the
// repository publishes it, and the local replace directive in its go.mod makes
// installing it from a module proxy impossible on purpose.
//
// It is thin by rule. Every flag maps onto one public library option. There is
// no transfer logic here, no knowledge of the artifact format, no retry,
// resume, or authentication logic, and no interface of its own.
//
// # Commands
//
//	bigoci push [flags] <file> <ref>
//	bigoci pull [flags] <ref> <dest>
//	bigoci help [push|pull]
//
// A flag that was not set passes nothing to the library, so the library's own
// default applies and the CLI never restates it. That is why "-title" set to
// the empty string is not the same as no "-title" at all: the first clears the
// file name annotation, the second lets the library name the artifact after
// the file it read.
//
// # Output contract
//
// Standard output carries data only. A push writes exactly one line, the
// digest of the manifest it wrote, and writes nothing at all when it fails. A
// pull writes nothing either way. Help asked for by name goes to standard
// output and exits zero.
//
// Everything else goes to standard error, every line prefixed "bigoci: "
// except the request log, whose lines are prefixed "http> ", "http< ", and
// "http! ". There is no terminal detection, no color, and no line rewriting,
// so the output is byte-identical piped and interactive.
//
// # Exit codes
//
//	0    success
//	1    failure, no sentinel matched
//	2    usage error
//	3    errors.Is(err, bigoci.ErrNotFound)
//	4    errors.Is(err, bigoci.ErrNotBigociArtifact)
//	5    errors.Is(err, bigoci.ErrDigestMismatch)
//	6    reserved: unauthorized
//	7    reserved: part too large
//	130  interrupted by SIGINT
//	143  terminated by SIGTERM
//
// Codes 6 and 7 are reserved now so the phases that raise those failures add
// rows without renumbering the ones a script already depends on.
//
// A failure always prints two lines. The first is the library's error verbatim,
// never re-wrapped and never re-phrased. The second is unconditional, because
// it is how a shell script watches the library's error classification work, and
// it takes one of three forms: the sentinel [errors.Is] matched and the code it
// maps to, the statement that none matched, or the signal that stopped the run,
// written "interrupted by SIGINT (exit 130)" or "terminated by SIGTERM
// (exit 143)".
//
// A recorded signal outranks the error's shape. Cancelling a transfer surfaces
// as whatever unwound first — a cancelled context, a closed file, a reset
// socket, sometimes an error a sentinel matches — and none of that changes why
// the run stopped, so the signal is what the second line reports.
//
// A usage error is the exception: it prints its complaint and then the
// offending command's usage block, and exits 2.
//
// # The request log
//
// The "-debug" flag installs an observer around the transport and logs one
// line when a request goes out and one when its response headers arrive. The
// format is a frozen contract: recipes grep it, so renaming a field is a
// breaking change.
//
//	http> <seq> <t> <METHOD> <URL> class=<class> auth=<auth> clen=<n> type=<v> range=<v> accept=<v>
//	http< <seq> <t> <METHOD> <URL> class=<class> status=<code> dur=<d> clen=<n> ctype=<v> \
//	      crange=<v> loc=<v> ddigest=<v> retry-after=<v> challenge=<v>
//	http! <seq> <t> <METHOD> <URL> class=<class> dur=<d> err=<v>
//
// One summary line follows the transfer, before the line that says how it
// ended. Its shape is fixed: every request class prints every time, zero or
// not, so a gate reads the zero it expects rather than inferring it from a
// field that is not there.
//
// The observer never touches a request, never reads a body in either
// direction, and renders only the headers it names one by one. A credential is
// unrepresentable in the output: the Authorization header shows its scheme and
// nothing else. Query parameter values are elided and their names escaped
// again on the way out; the one value that passes through is a "digest" whose
// bytes verifiably are a sha256 digest, which is checked on the value and never
// on the parameter's name. See README.md for the whole grammar and the recipes
// that read it.
//
// # Extraction trigger
//
// This package stays a single package main for as long as it stays thin. The
// trigger to split it is the day a file here has to know something about the
// artifact format, or the day a flag stops mapping onto one library option.
// Either means the library is missing something, and the fix belongs there.
package main
