package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// main wires the process to run: the real streams and arguments become the
// inputs, the terminating signals become a cancelled context, and the exit
// code run returns becomes the process status.
//
// This is the only function in the package that touches the process itself,
// so every other line of the harness is reachable from a test with buffers
// in place of the real streams.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)

	stop()
	os.Exit(code)
}
