package healthcheck

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"wal_scratch/follower"
	"wal_scratch/leader"
	"wal_scratch/router"
)

// newTestLeader starts a real HTTP test server backed by a leader node.
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

// newTestFollower creates a follower and its httptest.Server.
func newTestFollower(t *testing.T, leaderURL string) (*follower.Follower, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	f, err := follower.NewFollower(filepath.Join(dir, "follower.log"), leaderURL)
	if err != nil {
		t.Fatalf("failed to create follower: %v", err)
	}
	mux := http.NewServeMux()
	f.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		f.Close()
	})
	return f, ts
}

// put sends a PUT request to the given base URL and key.
func put(t *testing.T, baseURL, key, value string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"value": value})
	req, _ := http.NewRequest(http.MethodPut, baseURL+"/keys/"+key, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s/keys/%s failed: %v", baseURL, key, err)
	}
	defer resp.Body.Close()
}

// TestHealthCheckPassesWhenLeaderUp verifies that a reachable leader does not
// increment the failure counter.
func TestHealthCheckPassesWhenLeaderUp(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	_, followerTS := newTestFollower(t, leaderTS.URL)

	ro := router.NewRouter(leaderTS.URL, []string{followerTS.URL}, 30*time.Second)
	hc := NewHealthChecker(ro, []string{followerTS.URL}, time.Second, 3)

	hc.check()

	hc.mu.Lock()
	fails := hc.fails
	hc.mu.Unlock()

	if fails != 0 {
		t.Errorf("expected 0 consecutive failures when leader is up, got %d", fails)
	}
}

// TestFailureCounterIncrements verifies that pointing the checker at a
// non-existent URL increments consecutiveFails on each check() call.
func TestFailureCounterIncrements(t *testing.T) {
	_, followerTS := newTestFollower(t, "http://localhost:0")

	ro := router.NewRouter("http://localhost:0", []string{followerTS.URL}, 30*time.Second)
	hc := NewHealthChecker(ro, []string{followerTS.URL}, time.Second, 10)

	hc.check()
	hc.check()

	hc.mu.Lock()
	fails := hc.fails
	hc.mu.Unlock()

	if fails != 2 {
		t.Errorf("expected 2 consecutive failures, got %d", fails)
	}
}

// TestFailureCounterResetsOnSuccess verifies that a successful health check
// resets the failure counter to zero.
func TestFailureCounterResetsOnSuccess(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	_, followerTS := newTestFollower(t, leaderTS.URL)

	ro := router.NewRouter(leaderTS.URL, []string{followerTS.URL}, 30*time.Second)
	hc := NewHealthChecker(ro, []string{followerTS.URL}, time.Second, 10)

	// Manually set the counter to simulate prior failures.
	hc.mu.Lock()
	hc.fails = 5
	hc.mu.Unlock()

	hc.check()

	hc.mu.Lock()
	fails := hc.fails
	hc.mu.Unlock()

	if fails != 0 {
		t.Errorf("expected failures to reset to 0 after success, got %d", fails)
	}
}

// TestPromotionTriggersAfterThreshold verifies that once consecutiveFails
// reaches the threshold, a follower is promoted and the router's leader URL
// is updated.
func TestPromotionTriggersAfterThreshold(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	_, followerTS := newTestFollower(t, leaderTS.URL)

	ro := router.NewRouter(leaderTS.URL, []string{followerTS.URL}, 30*time.Second)
	hc := NewHealthChecker(ro, []string{followerTS.URL}, time.Second, 2)

	// Point the checker at a dead leader so checks fail.
	ro.SetLeader("http://localhost:0")

	hc.check()
	hc.check()

	if ro.ReturnleaderURL() == "http://localhost:0" {
		t.Error("expected router leader URL to be updated after promotion")
	}
	if ro.ReturnleaderURL() != followerTS.URL {
		t.Errorf("expected leader URL to be follower %s, got %s", followerTS.URL, ro.ReturnleaderURL())
	}
}

// TestMostUpToDateFollowerIsPromoted verifies that when multiple followers
// exist, the one with the highest applied_offset is selected for promotion.
func TestMostUpToDateFollowerIsPromoted(t *testing.T) {
	_, leaderTS := newTestLeader(t)

	f1, f1TS := newTestFollower(t, leaderTS.URL)
	f2, f2TS := newTestFollower(t, leaderTS.URL)

	// Write two entries to the leader.
	put(t, leaderTS.URL, "user:1", "alice")
	put(t, leaderTS.URL, "user:2", "bob")

	// f1 polls twice — picks up both entries (offsets 0 and 1).
	if err := f1.Poll(); err != nil {
		t.Fatalf("f1 first poll failed: %v", err)
	}
	if err := f1.Poll(); err != nil {
		t.Fatalf("f1 second poll failed: %v", err)
	}

	// f2 polls once — picks up only the first entry (offset 0).
	if err := f2.Poll(); err != nil {
		t.Fatalf("f2 poll failed: %v", err)
	}

	followerURLs := []string{f1TS.URL, f2TS.URL}
	ro := router.NewRouter("http://localhost:0", followerURLs, 30*time.Second)
	hc := NewHealthChecker(ro, followerURLs, time.Second, 1)

	// One failed check hits the threshold of 1.
	hc.check()

	if ro.ReturnleaderURL() != f1TS.URL {
		t.Errorf("expected f1 (%s) to be promoted, got %s", f1TS.URL, ro.ReturnleaderURL())
	}
	if f2.IsPromoted() {
		t.Error("f2 should not have been promoted — it was behind f1")
	}
}

// TestPromotedFollowerAcceptsWrites verifies that after promotion, a follower
// responds to PUT requests with 200 instead of 405.
func TestPromotedFollowerAcceptsWrites(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	f, _ := newTestFollower(t, leaderTS.URL)

	// Confirm writes are rejected before promotion via the mux.
	muxBefore := http.NewServeMux()
	f.RegisterRoutes(muxBefore)
	body, _ := json.Marshal(map[string]string{"value": "alice"})
	req, _ := http.NewRequest(http.MethodPut, "http://unused/keys/user:1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("key", "user:1")
	w := httptest.NewRecorder()
	muxBefore.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 before promotion, got %d", w.Code)
	}

	// Promote and re-register routes.
	f.Promote()

	muxAfter := http.NewServeMux()
	f.RegisterRoutes(muxAfter)

	body2, _ := json.Marshal(map[string]string{"value": "alice"})
	req2, _ := http.NewRequest(http.MethodPut, "http://unused/keys/user:1", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.SetPathValue("key", "user:1")
	w2 := httptest.NewRecorder()
	muxAfter.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 after promotion, got %d", w2.Code)
	}
}

// TestSplitBrainObservation demonstrates the split-brain scenario. After
// promotion, both the old leader and the promoted follower accept writes,
// resulting in diverged state. This test logs the divergence rather than
// asserting a failure — the point is to observe it.
func TestSplitBrainObservation(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	f, _ := newTestFollower(t, leaderTS.URL)

	// Write one entry and replicate it before the failover.
	put(t, leaderTS.URL, "user:1", "alice")
	if err := f.Poll(); err != nil {
		t.Fatalf("poll failed: %v", err)
	}

	// Simulate failover — promote the follower.
	f.Promote()

	mux := http.NewServeMux()
	f.RegisterRoutes(mux)
	promotedTS := httptest.NewServer(mux)
	t.Cleanup(promotedTS.Close)

	// Old leader is still running. Write to it directly (split-brain write 1).
	put(t, leaderTS.URL, "user:2", "old-leader-write")

	// Write to the promoted follower (split-brain write 2).
	put(t, promotedTS.URL, "user:3", "promoted-follower-write")

	// Read from both to observe the divergence.
	user3OnOldLeader, _ := http.Get(leaderTS.URL + "/keys/user:3")
	user2OnNewLeader, _ := http.Get(promotedTS.URL + "/keys/user:2")
	user3OnOldLeader.Body.Close()
	user2OnNewLeader.Body.Close()

	t.Logf("Split-brain observation:")
	t.Logf("  old leader:        has user:2=old-leader-write,       missing user:3 (status: %d)", user3OnOldLeader.StatusCode)
	t.Logf("  promoted follower: has user:3=promoted-follower-write, missing user:2 (status: %d)", user2OnNewLeader.StatusCode)
	t.Logf("  Both should be 404 — each node is missing the other's write.")
	t.Logf("  This is split-brain. This is why Raft exists.")
}
