package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"time"
)

// rowSchema versions the result row shape. Bump it when a field changes
// meaning, so old JSONL files cannot be summarized as if they were new.
const rowSchema = 1

// row is one measurement: one timed scenario of one iteration of one cell.
// Rows are self-contained on purpose — a JSONL file needs no spec beside it
// to be read.
type row struct {
	// Schema is the row shape's version.
	Schema int `json:"schema"`
	// RunID names the run the row belongs to.
	RunID string `json:"run_id"`
	// Timestamp is when the measurement finished, in UTC.
	Timestamp time.Time `json:"ts"`
	// CellID is the cell the row measures.
	CellID string `json:"cell_id"`
	// Registry is the cell's target name.
	Registry string `json:"registry"`
	// Scenario is the timed phase: cold-push, warm-push, or cold-pull.
	Scenario string `json:"scenario"`
	// PartSize is the part size in bytes.
	PartSize int64 `json:"part_size"`
	// Workers is the worker count.
	Workers int `json:"workers"`
	// FileSize is the transfer size in bytes.
	FileSize int64 `json:"file_size"`
	// Parts is how many parts the file split into.
	Parts int64 `json:"parts"`
	// Iteration numbers the repeat, starting at zero.
	Iteration int `json:"iteration"`
	// WallMS is the phase's wall-clock time in milliseconds.
	WallMS int64 `json:"wall_ms"`
	// MBPerS is the derived throughput in decimal megabytes per second,
	// the unit the design's per-connection expectations are quoted in.
	MBPerS float64 `json:"mb_per_s"`
	// HTTPStatus counts responses outside the 2xx and 3xx families during
	// the phase, keyed by status code. Empty means a clean phase.
	HTTPStatus map[string]int `json:"http_status,omitempty"`
	// Error is why the phase failed, empty on success. A failed row keeps
	// its wall time but carries no throughput.
	Error string `json:"error,omitempty"`
	// Commit is the harness build's short VCS revision, so a saved JSONL
	// names the code that produced it.
	Commit string `json:"commit,omitempty"`
}

// key returns the identity resume bookkeeping uses: a scenario of an
// iteration of a cell, measured at most once per output file.
func (r row) key() string {
	return r.CellID + "|" + r.Scenario + "|" + strconv.Itoa(r.Iteration)
}

// rowWriter appends rows to a JSONL file, flushing each one, so a killed
// run loses at most the row in flight.
type rowWriter struct {
	// file is the open output file.
	file *os.File
	// buf batches the encoder's writes; flushed per row.
	buf *bufio.Writer
	// encoder writes one row per line.
	encoder *json.Encoder
}

// newRowWriter opens path for appending, creating it if needed.
func newRowWriter(path string) (*rowWriter, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open results: %w", err)
	}

	buf := bufio.NewWriter(file)

	return &rowWriter{file: file, buf: buf, encoder: json.NewEncoder(buf)}, nil
}

// write appends one row and flushes it to the file.
func (w *rowWriter) write(r row) error {
	if err := w.encoder.Encode(r); err != nil {
		return fmt.Errorf("encode row %s: %w", r.key(), err)
	}
	if err := w.buf.Flush(); err != nil {
		return fmt.Errorf("flush row %s: %w", r.key(), err)
	}

	return nil
}

// close flushes and closes the output file.
func (w *rowWriter) close() error {
	flushErr := w.buf.Flush()
	closeErr := w.file.Close()
	if flushErr != nil {
		return fmt.Errorf("flush results: %w", flushErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close results: %w", closeErr)
	}

	return nil
}

// readRows parses every row in the JSONL file at path. Rows from a newer
// schema than this build knows are an error rather than a misreading.
func readRows(path string) ([]row, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open results: %w", err)
	}
	defer file.Close()

	var rows []row
	decoder := json.NewDecoder(file)
	for {
		var r row
		if err := decoder.Decode(&r); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parse results %s row %d: %w", path, len(rows)+1, err)
		}
		if r.Schema > rowSchema {
			return nil, fmt.Errorf(
				"results %s row %d: schema %d is newer than this build understands (%d)",
				path, len(rows)+1, r.Schema, rowSchema,
			)
		}
		rows = append(rows, r)
	}

	return rows, nil
}

// completedKeys returns the identity of every successful row already in the
// output file at path, which is what -resume skips. Failed rows are left
// out, so a re-run measures them again. A missing file means a fresh run.
func completedKeys(path string) (map[string]bool, error) {
	rows, err := readRows(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}

	keys := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.Error == "" {
			keys[r.key()] = true
		}
	}

	return keys, nil
}

// buildCommit returns the short VCS revision baked into the build, or empty
// when the build carries none. A "+" suffix marks a dirty working tree.
func buildCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	const short = 12
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				modified = "+"
			}
		}
	}
	if len(revision) > short {
		revision = revision[:short]
	}

	return revision + modified
}
