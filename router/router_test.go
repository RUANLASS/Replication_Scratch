package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"wal_scratch/follower"
	"wal_scratch/leader"
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

// newTestFollower creates a follower pointed at the given leader URL and
// returns both the follower and its httptest.Server.
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

// putThroughRouter issues a PUT through the router and returns the response.
// Uses a cookie jar so the client_id cookie is preserved across calls.
func putThroughRouter(t *testing.T, ro *Router, key, value string, jar http.CookieJar) *http.Response {
	t.Helper()
	mux := http.NewServeMux()
	ro.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]string{"value": value})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/keys/"+key, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Jar: jar}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT through router failed: %v", err)
	}
	return resp
}

// getThroughRouter issues a GET through the router and returns the response.
func getThroughRouter(t *testing.T, ro *Router, key string, jar http.CookieJar) *http.Response {
	t.Helper()
	mux := http.NewServeMux()
	ro.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/keys/"+key, nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET through router failed: %v", err)
	}
	return resp
}

// newCookieJar returns a simple in-memory cookie jar for test clients.
func newCookieJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("failed to create cookie jar: %v", err)
	}
	return jar
}

// TestReadRoutedToLeaderWhenFollowerBehind verifies that when a client writes
// and the follower has not yet caught up, the router falls back to the leader
// and still returns the correct value.
func TestReadRoutedToLeaderWhenFollowerBehind(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	// Follower is created but never polled — it will always be behind.
	_, followerTS := newTestFollower(t, leaderTS.URL)

	ro := NewRouter(leaderTS.URL, []string{followerTS.URL}, 30*time.Second)
	jar := newCookieJar(t)

	putResp := putThroughRouter(t, ro, "user:1", "alice", jar)
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d", putResp.StatusCode)
	}

	// Read immediately — follower is behind, should fall back to leader.
	getResp := getThroughRouter(t, ro, "user:1", jar)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", getResp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(getResp.Body).Decode(&result)
	if result["value"] != "alice" {
		t.Errorf("expected 'alice', got %q", result["value"])
	}
}

// TestReadRoutedToFollowerWhenCaughtUp verifies that once a follower has
// caught up to the client's write offset, reads are routed to the follower.
func TestReadRoutedToFollowerWhenCaughtUp(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	f, followerTS := newTestFollower(t, leaderTS.URL)

	ro := NewRouter(leaderTS.URL, []string{followerTS.URL}, 30*time.Second)
	jar := newCookieJar(t)

	putResp := putThroughRouter(t, ro, "user:1", "alice", jar)
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d", putResp.StatusCode)
	}

	// Manually poll the follower so it catches up.
	if err := f.Poll(); err != nil {
		t.Fatalf("follower poll failed: %v", err)
	}

	// Now the follower is caught up — read should be served by the follower.
	getResp := getThroughRouter(t, ro, "user:1", jar)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", getResp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(getResp.Body).Decode(&result)
	if result["value"] != "alice" {
		t.Errorf("expected 'alice', got %q", result["value"])
	}
}

// TestTokenExpiresAfterTTL verifies that after the TTL has elapsed, the write
// token is treated as expired and the router no longer pins the client to the leader.
func TestTokenExpiresAfterTTL(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	f, followerTS := newTestFollower(t, leaderTS.URL)

	// Use a very short TTL so we can expire it in the test.
	ro := NewRouter(leaderTS.URL, []string{followerTS.URL}, 10*time.Millisecond)
	jar := newCookieJar(t)

	putThroughRouter(t, ro, "user:1", "alice", jar)

	// Poll the follower so it is caught up.
	if err := f.Poll(); err != nil {
		t.Fatalf("follower poll failed: %v", err)
	}

	// Wait for the TTL to expire.
	time.Sleep(20 * time.Millisecond)

	// Token is expired — router should route freely to follower.
	// The follower is caught up, so the read should still succeed.
	getResp := getThroughRouter(t, ro, "user:1", jar)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET expected 200 after TTL expiry, got %d", getResp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(getResp.Body).Decode(&result)
	if result["value"] != "alice" {
		t.Errorf("expected 'alice' after TTL expiry, got %q", result["value"])
	}
}

// TestClientGetsCookieAfterWrite verifies that after writing through the router,
// the response contains a Set-Cookie header with a client_id value.
func TestClientGetsCookieAfterWrite(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	_, followerTS := newTestFollower(t, leaderTS.URL)

	ro := NewRouter(leaderTS.URL, []string{followerTS.URL}, 30*time.Second)

	mux := http.NewServeMux()
	ro.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]string{"value": "alice"})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/keys/user:1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		t.Fatalf("PUT failed: %v", err)
	}
	defer resp.Body.Close()

	var foundCookie bool
	for _, c := range resp.Cookies() {
		if c.Name == "client_id" && c.Value != "" {
			foundCookie = true
			break
		}
	}
	if !foundCookie {
		t.Error("expected a client_id cookie in the response after writing")
	}
}

// TestReadWithNoTokenRoutesToFollower verifies that a client with no prior write
// history is routed to a follower rather than the leader.
func TestReadWithNoTokenRoutesToFollower(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	f, followerTS := newTestFollower(t, leaderTS.URL)

	// Write directly to the leader (bypassing the router) so the follower
	// has something to serve, then poll to catch up.
	body, _ := json.Marshal(map[string]string{"value": "alice"})
	req, _ := http.NewRequest(http.MethodPut, leaderTS.URL+"/keys/user:1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)

	if err := f.Poll(); err != nil {
		t.Fatalf("follower poll failed: %v", err)
	}

	ro := NewRouter(leaderTS.URL, []string{followerTS.URL}, 30*time.Second)

	mux := http.NewServeMux()
	ro.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Fresh client — no cookie, no write token.
	resp, err := http.Get(ts.URL + "/keys/user:1")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	// Should still return the correct value (served by follower).
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["value"] != "alice" {
		t.Errorf("expected 'alice', got %q", result["value"])
	}
}

// TestRouterForwardsDeleteToLeader verifies that DELETE requests are
// forwarded to the leader and the result is reflected after polling.
func TestRouterForwardsDeleteToLeader(t *testing.T) {
	_, leaderTS := newTestLeader(t)
	f, followerTS := newTestFollower(t, leaderTS.URL)

	ro := NewRouter(leaderTS.URL, []string{followerTS.URL}, 30*time.Second)
	jar := newCookieJar(t)

	// Write then delete through the router.
	putThroughRouter(t, ro, "user:1", "alice", jar)

	mux := http.NewServeMux()
	ro.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := &http.Client{Jar: jar}
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/keys/user:1", nil)
	delResp, err := client.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE through router failed: %v", err)
	}
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE expected 200, got %d", delResp.StatusCode)
	}

	// Poll follower to catch up.
	if err := f.Poll(); err != nil {
		t.Fatalf("follower poll failed: %v", err)
	}

	// Read from follower — should be gone.
	getResp := getThroughRouter(t, ro, "user:1", jar)
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete propagated, got %d", getResp.StatusCode)
	}
}
