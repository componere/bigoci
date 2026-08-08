package main

import (
	"context"
	"os"
)

// main wires the process to run: the real streams and arguments become an env,
// the terminating signals become a cancelled context, and the exit code run
// returns becomes the process status.
//
// This is the only function in the package that touches the process itself, so
// every other line of the CLI is reachable from a test with buffers in place of
// the real streams.
func main() {
	ctx, cancel := context.WithCancel(context.Background())

	sig := watchSignals(cancel, os.Stderr)
	code := run(ctx, env{args: os.Args[1:], stdout: os.Stdout, stderr: os.Stderr}, sig)

	cancel()
	os.Exit(code)
}
