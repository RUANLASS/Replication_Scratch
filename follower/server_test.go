package follower

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"wal_scratch/leader"
)

// newTestLeader starts a real HTTP test server backed by a leader node.
// Returns the leader server and the httptest.Server (call ts.Close() to shut down).
func newTestLeader(t *testing.T) (*leader.Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	l, err := leader.NewServer(filepath.Join(dir, "leader.log"))
	if err != nil {
		t.Fatalf("failed to create leader: %v", err)
	}
	mux := http.NewServeMux()
	l.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
	})
	return l, ts
}

// newTestFollower creates a follower pointed at the given leader URL.
// Does not call Start() — tests drive polling manually via poll().
func newTestFollower(t *testing.T, leaderURL string) *Follower {
	t.Helper()
	dir := t.TempDir()
	f, err := NewFollower(filepath.Join(dir, "follower.log"), leaderURL)
	if err != nil {
		t.Fatalf("failed to create follower: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// putOnLeader writes a key/value to the leader via HTTP.
func putOnLeader(t *testing.T, leaderURL, key, value string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"value": value})
	req, _ := http.NewRequest(http.MethodPut, leaderURL+"/keys/"+key, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT to leader failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d", resp.StatusCode)
	}
}

// deleteOnLeader deletes a key from the leader via HTTP.
func deleteOnLeader(t *testing.T, leaderURL, key string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, leaderURL+"/keys/"+key, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE to leader failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE expected 200, got %d", resp.StatusCode)
	}
}

// getFromFollower issues a GET to the follower's handleGet handler directly.
func getFromFollower(t *testing.T, f *Follower, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/keys/"+key, nil)
	r.SetPathValue("key", key)
	w := httptest.NewRecorder()
	f.handleGet(w, r)
	return w
}

// TestFollowerReadsAfterPoll verifies that after polling the leader once,
// the follower serves the correct value for a key that was written to the leader.
func TestFollowerReadsAfterPoll(t *testing.T) {
	_, ts := newTestLeader(t)
	putOnLeader(t, ts.URL, "user:1", "alice")

	f := newTestFollower(t, ts.URL)
	if err := f.Poll(); err != nil {
		t.Fatalf("poll failed: %v", err)
	}

	resp := getFromFollower(t, f, "user:1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Code)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["value"] != "alice" {
		t.Errorf("expected 'alice', got %q", result["value"])
	}
}

// TestFollowerAppliesDelete verifies that a delete on the leader is reflected
// on the follower after a subsequent poll.
func TestFollowerAppliesDelete(t *testing.T) {
	_, ts := newTestLeader(t)
	putOnLeader(t, ts.URL, "user:1", "alice")

	f := newTestFollower(t, ts.URL)

	// First poll picks up the set.
	if err := f.Poll(); err != nil {
		t.Fatalf("first poll failed: %v", err)
	}

	// Delete on the leader, then poll again.
	deleteOnLeader(t, ts.URL, "user:1")
	if err := f.Poll(); err != nil {
		t.Fatalf("second poll failed: %v", err)
	}

	resp := getFromFollower(t, f, "user:1")
	if resp.Code != http.StatusNotFound {
		t.Errorf("expected 404 after delete propagated, got %d", resp.Code)
	}
}

// TestFollowerOffsetAdvances verifies that appliedOffset increases correctly
// as the follower processes entries from the leader.
func TestFollowerOffsetAdvances(t *testing.T) {
	_, ts := newTestLeader(t)
	putOnLeader(t, ts.URL, "user:1", "alice")
	putOnLeader(t, ts.URL, "user:2", "bob")
	putOnLeader(t, ts.URL, "user:3", "carol")

	f := newTestFollower(t, ts.URL)
	if err := f.Poll(); err != nil {
		t.Fatalf("poll failed: %v", err)
	}

	// Three entries were written (offsets 0, 1, 2).
	// After polling, appliedOffset should be 2.
	if f.AppliedOffset != 2 {
		t.Errorf("expected appliedOffset 2, got %d", f.AppliedOffset)
	}
}

// TestFollowerRecovery verifies that a follower correctly restores its
// appliedOffset after being closed and reopened.
func TestFollowerRecovery(t *testing.T) {
	_, ts := newTestLeader(t)
	putOnLeader(t, ts.URL, "user:1", "alice")
	putOnLeader(t, ts.URL, "user:2", "bob")

	dir := t.TempDir()
	walPath := filepath.Join(dir, "follower.log")

	// First follower session: poll and close.
	f1, err := NewFollower(walPath, ts.URL)
	if err != nil {
		t.Fatalf("failed to create first follower: %v", err)
	}
	if err := f1.Poll(); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	f1.Close()

	// Second follower session: reopen and verify offset is restored.
	f2, err := NewFollower(walPath, ts.URL)
	if err != nil {
		t.Fatalf("failed to create second follower: %v", err)
	}
	defer f2.Close()

	// Two entries were written (offsets 0 and 1).
	// After recovery, appliedOffset should be 1.
	if f2.AppliedOffset != 1 {
		t.Errorf("expected appliedOffset 1 after recovery, got %d", f2.AppliedOffset)
	}

	// In-memory store should also be reconstructed.
	resp := getFromFollower(t, f2, "user:1")
	if resp.Code != http.StatusOK {
		t.Errorf("expected user:1 to survive follower restart, got %d", resp.Code)
	}
}

// TestFollowerRejectsWrites verifies that the follower returns 405 for
// any attempt to write via PUT or DELETE.
func TestFollowerRejectsWrites(t *testing.T) {
	_, ts := newTestLeader(t)
	f := newTestFollower(t, ts.URL)

	mux := http.NewServeMux()
	f.RegisterRoutes(mux)

	// PUT should be rejected.
	body, _ := json.Marshal(map[string]string{"value": "alice"})
	putReq := httptest.NewRequest(http.MethodPut, "/keys/user:1", bytes.NewReader(body))
	putResp := httptest.NewRecorder()
	mux.ServeHTTP(putResp, putReq)
	if putResp.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT expected 405, got %d", putResp.Code)
	}

	// DELETE should be rejected.
	delReq := httptest.NewRequest(http.MethodDelete, "/keys/user:1", nil)
	delResp := httptest.NewRecorder()
	mux.ServeHTTP(delResp, delReq)
	if delResp.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE expected 405, got %d", delResp.Code)
	}
}

// TestFollowerStatus verifies the /status endpoint returns the correct
// leader URL and applied offset.
func TestFollowerStatus(t *testing.T) {
	_, ts := newTestLeader(t)
	putOnLeader(t, ts.URL, "user:1", "alice")

	f := newTestFollower(t, ts.URL)
	if err := f.Poll(); err != nil {
		t.Fatalf("poll failed: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	f.handleStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result map[string]interface{}
	json.NewDecoder(w.Body).Decode(&result)

	if result["leader_url"] != ts.URL {
		t.Errorf("expected leader_url %q, got %q", ts.URL, result["leader_url"])
	}
	if result["applied_offset"] == nil {
		t.Error("expected applied_offset in status response")
	}
}
