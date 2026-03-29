package wal

import (
	"path/filepath"
	"testing"
)

// TestAppendAndReadAll verifies that entries appended to the WAL
// can be read back in order with correct fields.
func TestAppendAndReadAll(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("failed to open WAL: %v", err)
	}
	defer w.Close()

	w.Append("set", "user:1", "alice")
	w.Append("set", "user:2", "bob")
	w.Append("delete", "user:1", "")

	entries, err := w.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	expected := []struct {
		op    string
		key   string
		value string
	}{
		{"set", "user:1", "alice"},
		{"set", "user:2", "bob"},
		{"delete", "user:1", ""},
	}

	for i, e := range entries {
		if e.Op != expected[i].op {
			t.Errorf("entry %d: expected op %q, got %q", i, expected[i].op, e.Op)
		}
		if e.Key != expected[i].key {
			t.Errorf("entry %d: expected key %q, got %q", i, expected[i].key, e.Key)
		}
		if e.Value != expected[i].value {
			t.Errorf("entry %d: expected value %q, got %q", i, expected[i].value, e.Value)
		}
	}
}

// TestOffsetAssignment verifies that offsets are assigned sequentially
// starting from 0.
func TestOffsetAssignment(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("failed to open WAL: %v", err)
	}
	defer w.Close()

	for i := 0; i < 5; i++ {
		w.Append("set", "key", "value")
	}

	entries, err := w.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}

	if len(entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(entries))
	}

	for i, e := range entries {
		if e.Offset != uint64(i) {
			t.Errorf("entry %d: expected offset %d, got %d", i, i, e.Offset)
		}
	}
}

// TestReadFrom verifies that ReadFrom returns only entries at or after
// the given offset.
func TestReadFrom(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("failed to open WAL: %v", err)
	}
	defer w.Close()

	for i := 0; i < 5; i++ {
		w.Append("set", "key", "value")
	}

	entries, err := w.ReadFrom(3)
	if err != nil {
		t.Fatalf("ReadFrom failed: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries from offset 3, got %d", len(entries))
	}

	if entries[0].Offset != 3 {
		t.Errorf("expected first entry offset 3, got %d", entries[0].Offset)
	}
	if entries[1].Offset != 4 {
		t.Errorf("expected second entry offset 4, got %d", entries[1].Offset)
	}
}

// TestCrashRecovery is the most important test. It verifies that reopening
// an existing WAL file correctly resumes offset assignment from where it
// left off, rather than restarting from 0.
func TestCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	// First session: open, write three entries, close.
	w, err := Open(path)
	if err != nil {
		t.Fatalf("failed to open WAL (first session): %v", err)
	}
	w.Append("set", "user:1", "alice")
	w.Append("set", "user:2", "bob")
	w.Append("set", "user:3", "carol")
	w.Close()

	// Second session: reopen the same file, write one more entry.
	w2, err := Open(path)
	if err != nil {
		t.Fatalf("failed to open WAL (second session): %v", err)
	}
	defer w2.Close()

	offset, err := w2.Append("set", "user:4", "dave")
	if err != nil {
		t.Fatalf("Append failed in second session: %v", err)
	}

	// The fourth entry should have offset 3, not 0.
	if offset != 3 {
		t.Errorf("expected offset 3 after recovery, got %d — Open() is not scanning existing entries", offset)
	}

	// ReadAll should return all four entries.
	entries, err := w2.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll failed after recovery: %v", err)
	}

	if len(entries) != 4 {
		t.Fatalf("expected 4 entries after recovery, got %d", len(entries))
	}

	// Verify all offsets are sequential with no gaps or duplicates.
	for i, e := range entries {
		if e.Offset != uint64(i) {
			t.Errorf("entry %d: expected offset %d, got %d", i, i, e.Offset)
		}
	}
}

// TestAppendReturnsOffset verifies that Append returns the correct offset
// for each entry as it is written.
func TestAppendReturnsOffset(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("failed to open WAL: %v", err)
	}
	defer w.Close()

	for i := 0; i < 3; i++ {
		offset, err := w.Append("set", "key", "value")
		if err != nil {
			t.Fatalf("Append failed: %v", err)
		}
		if offset != uint64(i) {
			t.Errorf("expected Append to return offset %d, got %d", i, offset)
		}
	}
}
