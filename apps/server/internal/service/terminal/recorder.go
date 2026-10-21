package terminal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Recorder writes a terminal session to an asciicast v2 file.
//
// # Why record at all
//
// Because the confidentiality trade has already been made. `ssh -J` relays
// ciphertext and EdgePilot cannot read it; the browser terminal decrypts the
// session in order to draw it. Given that the server can see the session, the
// choice is between seeing it and keeping no account of it, or seeing it and
// keeping one. The second is the only version an operator can defend to a
// customer afterwards.
//
// # Why asciicast rather than a bespoke format
//
// It is a documented format with existing players, so a recording is useful
// without EdgePilot having to grow a viewer, and it stays readable if this
// project stops existing. It is newline-delimited JSON, so a recording of a
// session that was cut off mid-write is still valid up to the last complete
// line — which is exactly the case that matters, because sessions end badly far
// more often than they end tidily.
//
// # What is not recorded
//
// Keystrokes. Only what the terminal displayed is kept. Typed input is echoed
// by the remote shell and so appears in the output anyway, but a password
// prompt deliberately does not echo — and capturing input would turn every
// recording into a file of the customer's passwords. That is a line worth not
// crossing even inside a feature that has already given up confidentiality.
type Recorder struct {
	mu      sync.Mutex
	file    *os.File
	encoder *json.Encoder
	started time.Time
	closed  bool
}

// asciicastHeader is the first line of the file.
type asciicastHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title"`
	Env       map[string]string `json:"env,omitempty"`
}

// NewRecorder opens a recording for a session.
//
// Returns a nil Recorder when dir is empty, and every method tolerates a nil
// receiver — so the caller writes the same code whether recording is configured
// or not, rather than guarding each call.
func NewRecorder(dir, sessionID, title string, cols, rows int, now time.Time) (*Recorder, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("terminal: creating the recording directory: %w", err)
	}

	// 0600: a recording is a transcript of an administrative session on a
	// customer's machine, and it is readable by whoever can read the file.
	path := filepath.Join(dir, sessionID+".cast")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("terminal: creating the recording: %w", err)
	}

	r := &Recorder{file: file, encoder: json.NewEncoder(file), started: now}
	if err := r.encoder.Encode(asciicastHeader{
		Version:   2,
		Width:     cols,
		Height:    rows,
		Timestamp: now.Unix(),
		Title:     title,
		Env:       map[string]string{"TERM": "xterm-256color"},
	}); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("terminal: writing the recording header: %w", err)
	}
	return r, nil
}

// Output appends terminal output at its offset from the start of the session.
//
// Errors are returned but callers are expected to log and continue: a session
// that dies because its recording could not be written would be a worse
// outcome than a recording with a gap, and the gap is visible in the file.
func (r *Recorder) Output(at time.Time, data []byte) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}

	// [elapsed-seconds, "o", data] — the asciicast event form. Written through
	// json.Encoder so the payload is escaped correctly; terminal output is full
	// of control bytes and quotes.
	return r.encoder.Encode([]any{
		at.Sub(r.started).Seconds(), "o", string(data),
	})
}

// Resize records a terminal size change, so a player reflows as the operator did.
func (r *Recorder) Resize(at time.Time, cols, rows int) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	return r.encoder.Encode([]any{
		at.Sub(r.started).Seconds(), "r", fmt.Sprintf("%dx%d", cols, rows),
	})
}

// Close finishes the recording.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true

	// Synced before closing: a recording is evidence, and the case where it
	// matters most is the one where this process is about to die.
	_ = r.file.Sync()
	return r.file.Close()
}

// RecordingPath is where a session's recording lives.
func RecordingPath(dir, sessionID string) string {
	return filepath.Join(dir, sessionID+".cast")
}
