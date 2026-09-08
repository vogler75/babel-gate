package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRotator_WriteAndRotate(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "rotator_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	logFile := filepath.Join(tempDir, "test.log")
	// Set 1KB max size for testing rotation
	r := &Rotator{
		filename:   logFile,
		maxSize:    200, // 200 bytes max
		maxBackups: 3,
	}
	if err := r.openExistingOrNew(); err != nil {
		t.Fatalf("failed to open log file: %v", err)
	}
	defer r.Close()

	// Write 20 lines of ~46 bytes each = 920 bytes total with 200 bytes max per file
	line := "1234567890123456789012345678901234567890\n" // 41 bytes
	for i := 0; i < 20; i++ {
		_, err := r.Write([]byte(fmt.Sprintf("[%02d] %s", i, line)))
		if err != nil {
			t.Fatalf("failed to write line %d: %v", i, err)
		}
	}

	// Check files created
	if _, err := os.Stat(logFile); err != nil {
		t.Errorf("expected primary log file to exist: %v", err)
	}
	if _, err := os.Stat(logFile + ".1"); err != nil {
		t.Errorf("expected backup .1 to exist: %v", err)
	}
	if _, err := os.Stat(logFile + ".2"); err != nil {
		t.Errorf("expected backup .2 to exist: %v", err)
	}
	if _, err := os.Stat(logFile + ".3"); err != nil {
		t.Errorf("expected backup .3 to exist: %v", err)
	}
	// Check that backup .4 does NOT exist because maxBackups is 3
	if _, err := os.Stat(logFile + ".4"); err == nil {
		t.Errorf("expected backup .4 NOT to exist since maxBackups is 3")
	}
}

func TestRingBuffer(t *testing.T) {
	rb := NewRingBuffer(3)
	if rb.Count() != 0 {
		t.Errorf("expected count 0, got %d", rb.Count())
	}

	rb.Push("line 1")
	rb.Push("line 2")
	lines := rb.Lines()
	if len(lines) != 2 || lines[0] != "line 1" || lines[1] != "line 2" {
		t.Errorf("unexpected lines: %v", lines)
	}

	rb.Push("line 3")
	rb.Push("line 4") // should drop line 1

	lines = rb.Lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "line 2" || lines[1] != "line 3" || lines[2] != "line 4" {
		t.Errorf("unexpected rotated ring lines: %v", lines)
	}

	rb.Clear()
	if rb.Count() != 0 || len(rb.Lines()) != 0 {
		t.Errorf("expected empty buffer after clear")
	}
}

func TestMultiWriterWithRing(t *testing.T) {
	rb := NewRingBuffer(5)
	mw := NewMultiWriterWithRing(nil, rb)

	_, err := mw.Write([]byte("first line\nsecond line\npartia"))
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	lines := rb.Lines()
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0] != "first line" || lines[1] != "second line" {
		t.Errorf("unexpected lines: %v", lines)
	}

	_, err = mw.Write([]byte("l line\n"))
	if err != nil {
		t.Fatalf("failed to write second chunk: %v", err)
	}

	lines = rb.Lines()
	if len(lines) != 3 || lines[2] != "partial line" {
		t.Errorf("unexpected lines after finishing partial line: %v", lines)
	}
}

func TestRotator_ConcurrentWrites(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "rotator_concurrent_*")
	if err != nil {
		t.Fatalf("temp dir error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	logFile := filepath.Join(tempDir, "concurrent.log")
	r, err := NewRotator(RotatorOptions{
		Filename:   logFile,
		MaxSizeMB:  1,
		MaxBackups: 2,
	})
	if err != nil {
		t.Fatalf("failed to create rotator: %v", err)
	}
	defer r.Close()

	var wg sync.WaitGroup
	workers := 10
	iterations := 50

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _ = r.Write([]byte(fmt.Sprintf("worker %d line %d\n", workerID, j)))
			}
		}(i)
	}
	wg.Wait()
}
