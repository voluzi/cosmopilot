// Package tracer provides functionality for tracing blockchain store operations
// by reading and parsing trace output from a FIFO pipe.
package tracer

import (
	"context"
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/goccy/go-json"

	"github.com/containerd/fifo"
	"github.com/nxadm/tail"
)

const (
	// Keep enough of the current JSON object to identify the trace without
	// dumping potentially large base64 key/value payloads into logs.
	traceErrorObjectPrefixLength = 64
	// Keep a separate bounded window around parse failures outside the prefix.
	traceErrorNearWindowLength = 64
)

// StoreTracer reads and parses store operation traces from a file or FIFO pipe.
type StoreTracer struct {
	tail   *tail.Tail
	Traces chan *Trace
}

// Trace represents a single store operation trace event.
type Trace struct {
	Operation string    `json:"operation"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Metadata  *Metadata `json:"metadata,omitempty"`
	Err       error     `json:"-"`
}

// Metadata contains contextual information about a trace event.
type Metadata struct {
	BlockHeight int64  `json:"blockHeight"`
	StoreName   string `json:"store_name"`
}

// NewStoreTracer creates a new StoreTracer that reads from the specified path.
// If createFifo is true, a FIFO pipe will be created at the path.
// The tracer must be started with Start() to begin processing traces.
func NewStoreTracer(path string, createFifo bool) (*StoreTracer, error) {
	if createFifo {
		f, err := fifo.OpenFifo(context.Background(), path, syscall.O_CREAT|syscall.O_RDONLY|syscall.O_NONBLOCK, 0655)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}

	t, err := tail.TailFile(path, tail.Config{
		ReOpen: true,
		Pipe:   true,
		Follow: true,
		Logger: tail.DiscardingLogger,
	})
	if err != nil {
		return nil, err
	}

	return &StoreTracer{
		tail:   t,
		Traces: make(chan *Trace),
	}, nil
}

// Stop stops the tracer and closes the underlying file.
func (t *StoreTracer) Stop() error {
	return t.tail.Stop()
}

// Start begins processing trace events and sending them to the Traces channel.
// This method blocks until the tracer is stopped or the file is closed.
// The Traces channel is closed when Start returns, allowing consumers to detect completion.
func (t *StoreTracer) Start() {
	defer close(t.Traces)
	for line := range t.tail.Lines {
		if line.Err != nil {
			t.Traces <- &Trace{Err: line.Err}
			continue
		}

		text := strings.TrimSpace(line.Text)
		if text == "" {
			continue
		}

		decoder := json.NewDecoder(strings.NewReader(text))
		for {
			objectOffset := decoder.InputOffset()
			trace := Trace{}
			if err := decoder.Decode(&trace); err != nil {
				if err != io.EOF {
					errorOffset := decoder.InputOffset()
					objectPrefix, nearError := traceErrorContext(text, objectOffset, errorOffset)
					if nearError == "" {
						t.Traces <- &Trace{Err: fmt.Errorf(
							"failed to parse trace at byte %d of %d (object=%q): %w",
							errorOffset, len(text), objectPrefix, err,
						)}
					} else {
						t.Traces <- &Trace{Err: fmt.Errorf(
							"failed to parse trace at byte %d of %d (object=%q near=%q): %w",
							errorOffset, len(text), objectPrefix, nearError, err,
						)}
					}
				}
				break
			}
			t.Traces <- &trace
		}
	}
}

func traceErrorContext(text string, objectOffset, errorOffset int64) (string, string) {
	objectStart := clampOffset(objectOffset, len(text))
	for objectStart < len(text) && isJSONWhitespace(text[objectStart]) {
		objectStart++
	}
	objectEnd := min(objectStart+traceErrorObjectPrefixLength, len(text))

	nearCenter := clampOffset(errorOffset, len(text))
	nearStart := nearCenter - traceErrorNearWindowLength/2
	if nearStart < objectStart {
		nearStart = objectStart
	}
	nearEnd := nearStart + traceErrorNearWindowLength
	if nearEnd > len(text) {
		nearEnd = len(text)
		nearStart = max(objectStart, nearEnd-traceErrorNearWindowLength)
	}
	if nearStart < objectEnd {
		objectEnd = min(max(objectEnd, nearEnd), objectStart+traceErrorObjectPrefixLength+traceErrorNearWindowLength)
		objectPrefix := text[objectStart:objectEnd]
		if objectEnd < len(text) {
			objectPrefix += "..."
		}
		return objectPrefix, ""
	}

	objectPrefix := text[objectStart:objectEnd]
	if objectEnd < len(text) {
		objectPrefix += "..."
	}

	nearError := text[nearStart:nearEnd]
	if nearStart > objectStart {
		nearError = "..." + nearError
	}
	if nearEnd < len(text) {
		nearError += "..."
	}
	return objectPrefix, nearError
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '	' || value == '\r' || value == '\n'
}

func clampOffset(offset int64, length int) int {
	if offset < 0 {
		return 0
	}
	if offset > int64(length) {
		return length
	}
	return int(offset)
}
