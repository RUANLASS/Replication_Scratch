package leader

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := NewServer(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	return s
}

func newTestServerAt(t *testing.T, path string) *Server {
	t.Helper()
	s, err := NewServer(path)
	if err != nil {
		t.Fatalf("failed to create server at %s: %v", path, err)
	}
	return s
}

func put(t *testing.T, s *Server, key, value string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"value": value})
	r := httptest.NewRequest(http.MethodPut, "/keys/"+key, bytes.NewReader(body))
	r.SetPathValue("key", key)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleSet(w, r)
	return w
}

func get(t *testing.T, s *Server, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/keys/"+key, nil)
	r.SetPathValue("key", key)
	w := httptest.NewRecorder()
	s.handleGet(w, r)
	return w
}

func del(t *testing.T, s *Server, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/keys/"+key, nil)
	r.SetPathValue("key", key)
	w := httptest.NewRecorder()
	s.handleDelete(w, r)
	return w
}

// TestSetAndGet verifies that a key written via PUT can be read back via GET.
func TestSetAndGet(t *testing.T) {
	s := newTestServer(t)

	resp := put(t, s, "user:1", "alice")
	if resp.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d", resp.Code)
	}

	resp = get(t, s, "user:1")
	if resp.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", resp.Code)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)

	if result["value"] != "alice" {
		t.Errorf("expected value 'alice', got %q", result["value"])
	}
	if result["key"] != "user:1" {
		t.Errorf("expected key 'user:1', got %q", result["key"])
	}
}

// TestGetMissingKey verifies that GET on a nonexistent key returns 404.
func TestGetMissingKey(t *testing.T) {
	s := newTestServer(t)

	resp := get(t, s, "doesnotexist")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Code)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)

	if result["error"] == "" {
		t.Error("expected an error message in response body")
	}
}

// TestDelete verifies that a deleted key is no longer accessible.
func TestDelete(t *testing.T) {
	s := newTestServer(t)

	put(t, s, "user:1", "alice")

	resp := del(t, s, "user:1")
	if resp.Code != http.StatusOK {
		t.Fatalf("DELETE expected 200, got %d", resp.Code)
	}

	resp = get(t, s, "user:1")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE expected 404, got %d", resp.Code)
	}
}

// TestStateReconstruction is the critical test. It verifies that a new server
// instance pointing at the same WAL file reconstructs the correct state,
// including honouring deletes that occurred before the restart.
func TestStateReconstruction(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "test.log")

	// First server session: write some data and delete one key.
	s1 := newTestServerAt(t, walPath)
	put(t, s1, "user:1", "alice")
	put(t, s1, "user:2", "bob")
	del(t, s1, "user:1")

	// Second server session: open the same WAL and verify state.
	s2 := newTestServerAt(t, walPath)

	// user:1 was deleted — should be gone.
	resp := get(t, s2, "user:1")
	if resp.Code != http.StatusNotFound {
		t.Errorf("user:1 was deleted before restart — expected 404, got %d", resp.Code)
	}

	// user:2 was never deleted — should still be there.
	resp = get(t, s2, "user:2")
	if resp.Code != http.StatusOK {
		t.Fatalf("user:2 should survive restart — expected 200, got %d", resp.Code)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["value"] != "bob" {
		t.Errorf("expected user:2 value 'bob', got %q", result["value"])
	}
}

// TestInvalidRequestBody verifies that a malformed JSON body returns 400.
func TestInvalidRequestBody(t *testing.T) {
	s := newTestServer(t)

	r := httptest.NewRequest(http.MethodPut, "/keys/user:1", bytes.NewBufferString("not json"))
	r.SetPathValue("key", "user:1")
	w := httptest.NewRecorder()
	s.handleSet(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid body, got %d", w.Code)
	}
}
