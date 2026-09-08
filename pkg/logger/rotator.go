package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Rotator implements io.WriteCloser and manages size-based log file rotation.
type Rotator struct {
	mu         sync.Mutex
	filename   string
	maxSize    int64 // bytes
	maxBackups int
	size       int64
	file       *os.File
}

// RotatorOptions specifies settings for log rotation.
type RotatorOptions struct {
	Filename   string
	MaxSizeMB  int // megabytes before rotation
	MaxBackups int // number of rotated files to retain
}

// NewRotator creates a new size-based rotating log file writer.
func NewRotator(opts RotatorOptions) (*Rotator, error) {
	if opts.Filename == "" {
		opts.Filename = "logs/babelgate.log"
	}
	if opts.MaxSizeMB <= 0 {
		opts.MaxSizeMB = 10
	}
	if opts.MaxBackups < 0 {
		opts.MaxBackups = 5
	}

	dir := filepath.Dir(opts.Filename)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create log dir %s: %w", dir, err)
		}
	}

	r := &Rotator{
		filename:   opts.Filename,
		maxSize:    int64(opts.MaxSizeMB) * 1024 * 1024,
		maxBackups: opts.MaxBackups,
	}

	if err := r.openExistingOrNew(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Rotator) openExistingOrNew() error {
	info, err := os.Stat(r.filename)
	if err == nil {
		r.size = info.Size()
		f, err := os.OpenFile(r.filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("open log file %s: %w", r.filename, err)
		}
		r.file = f
		return nil
	}

	f, err := os.OpenFile(r.filename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create log file %s: %w", r.filename, err)
	}
	r.file = f
	r.size = 0
	return nil
}

// Write writes log bytes to the active log file, rotating if file exceeds maxSize.
func (r *Rotator) Write(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	writeLen := int64(len(p))
	if writeLen > r.maxSize {
		return 0, fmt.Errorf("write size %d exceeds max log file size %d", writeLen, r.maxSize)
	}

	if r.file == nil {
		if err := r.openExistingOrNew(); err != nil {
			return 0, err
		}
	}

	if r.size+writeLen > r.maxSize {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err = r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// rotateLocked performs log rotation: shifts old backups and re-opens current file.
func (r *Rotator) rotateLocked() error {
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}

	if r.maxBackups > 0 {
		// Remove the oldest backup if it exceeds maxBackups
		oldest := fmt.Sprintf("%s.%d", r.filename, r.maxBackups)
		_ = os.Remove(oldest)

		// Shift existing backups: .4 -> .5, .3 -> .4, etc.
		for i := r.maxBackups - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", r.filename, i)
			dst := fmt.Sprintf("%s.%d", r.filename, i+1)
			if _, err := os.Stat(src); err == nil {
				_ = os.Rename(src, dst)
			}
		}

		// Rename primary to .1
		dst := fmt.Sprintf("%s.1", r.filename)
		_ = os.Rename(r.filename, dst)
	} else {
		// No backups: just remove the primary file
		_ = os.Remove(r.filename)
	}

	return r.openExistingOrNew()
}

// Close closes the underlying active log file.
func (r *Rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		err := r.file.Close()
		r.file = nil
		return err
	}
	return nil
}

// RingBuffer is a thread-safe circular buffer for holding recent log entries.
type RingBuffer struct {
	mu       sync.RWMutex
	lines    []string
	capacity int
	head     int
	count    int
}

// NewRingBuffer creates a new ring buffer with fixed capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 500
	}
	return &RingBuffer{
		lines:    make([]string, capacity),
		capacity: capacity,
	}
}

// Push adds a new line into the ring buffer.
func (rb *RingBuffer) Push(line string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	idx := (rb.head + rb.count) % rb.capacity
	if rb.count < rb.capacity {
		rb.lines[idx] = line
		rb.count++
	} else {
		rb.lines[rb.head] = line
		rb.head = (rb.head + 1) % rb.capacity
	}
}

// Clear removes all stored lines.
func (rb *RingBuffer) Clear() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.head = 0
	rb.count = 0
}

// Lines returns a copy of all lines currently in the ring buffer in chronological order.
func (rb *RingBuffer) Lines() []string {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	result := make([]string, rb.count)
	for i := 0; i < rb.count; i++ {
		result[i] = rb.lines[(rb.head+i)%rb.capacity]
	}
	return result
}

// Count returns the current number of lines in the buffer.
func (rb *RingBuffer) Count() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.count
}

// MultiWriterWithRing writes incoming bytes to an optional io.Writer and a RingBuffer.
type MultiWriterWithRing struct {
	mu     sync.Mutex
	writer io.Writer
	ring   *RingBuffer
	buf    []byte
}

// NewMultiWriterWithRing creates a writer that streams to an io.Writer and stores lines in a RingBuffer.
func NewMultiWriterWithRing(w io.Writer, ring *RingBuffer) *MultiWriterWithRing {
	return &MultiWriterWithRing{
		writer: w,
		ring:   ring,
	}
}

// Write splits incoming stream by newlines, pushes full lines to ring buffer, and writes to underlying writer.
func (m *MultiWriterWithRing) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.writer != nil {
		if nw, wErr := m.writer.Write(p); wErr != nil {
			return nw, wErr
		}
	}

	if m.ring != nil {
		m.buf = append(m.buf, p...)
		for {
			idx := -1
			for i, b := range m.buf {
				if b == '\n' {
					idx = i
					break
				}
			}
			if idx == -1 {
				break
			}
			line := string(m.buf[:idx])
			m.ring.Push(line)
			m.buf = m.buf[idx+1:]
		}
	}

	return len(p), nil
}
