package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
	"wal_scratch/follower"
)

type writeToken struct {
	offset    uint64
	writtenAt time.Time
}

type Router struct {
	LeaderURL    string
	FollowerURLs []string
	Tokens       map[string]writeToken
	TokenTTL     time.Duration
	Mu           sync.RWMutex
}

func NewRouter(leaderURL string, FollowerURLs []string, ttl time.Duration) *Router {
	new_router := Router{
		LeaderURL:    leaderURL,
		FollowerURLs: FollowerURLs,
		Tokens:       make(map[string]writeToken),
		TokenTTL:     ttl,
	}
	return &new_router
}

func (ro *Router) tokenKey(r *http.Request) string {
	cookie, err := r.Cookie("client_id")
	if err == nil && cookie.Value != "" {
		return cookie.Value
	}

	return r.RemoteAddr

}

func (ro *Router) recordWrite(clientID string, offset uint64) {
	ro.Mu.Lock()
	new_wrttkn := writeToken{
		offset:    offset,
		writtenAt: time.Now(),
	}
	ro.Tokens[clientID] = new_wrttkn
	ro.Mu.Unlock()
}

func (ro *Router) selectBackend(r *http.Request) string {
	key := ro.tokenKey(r)
	ro.Mu.RLock()
	token, ok := ro.Tokens[key]
	ro.Mu.RUnlock()

	if !ok {
		return ro.LeaderURL
	}

	if time.Since(token.writtenAt) > ro.TokenTTL {
		ro.Mu.Lock()
		delete(ro.Tokens, key)
		ro.Mu.Unlock()
		for _, val := range ro.FollowerURLs {
			_, err := follower.FetchAppliedOffset(val)
			if err == nil {
				return val
			}
		}
		return ro.LeaderURL
	}

	for _, val := range ro.FollowerURLs {
		appOffset, err := follower.FetchAppliedOffset(val)
		if err != nil {
			continue
		}
		if appOffset >= token.offset {
			return val
		}
	}

	return ro.LeaderURL
}

func (ro *Router) handleWrite(w http.ResponseWriter, r *http.Request) {
	token := ro.tokenKey(r)

	http.SetCookie(w, &http.Cookie{
		Name:    "client_id",
		Value:   token,
		Expires: time.Now().Add(24 * time.Hour),
		Path:    "/",
	})

	target, _ := url.Parse(ro.LeaderURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewBuffer(body))

		var data struct {
			Offset uint64 `json:"offset"`
		}
		if err := json.Unmarshal(body, &data); err == nil {
			// 3. RECORD: Update the router's internal state
			ro.recordWrite(token, data.Offset)
		}
		return nil
	}

	proxy.ServeHTTP(w, r)
}

func (ro *Router) handleRead(w http.ResponseWriter, r *http.Request) {
	target := ro.selectBackend(r)
	targetURL, err := url.Parse(target)
	if err != nil {
		http.Error(w, "invalid backend URL", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	fmt.Printf("Routing READ for %s to %s\n", r.URL.Path, targetURL)
	proxy.ServeHTTP(w, r)
}

func (ro *Router) ReturnleaderURL() string {
	ro.Mu.RLock()
	defer ro.Mu.RUnlock()
	return ro.LeaderURL
}

func (ro *Router) SetLeader(newURL string) {
	ro.Mu.Lock()
	oldURL := ro.LeaderURL
	ro.LeaderURL = newURL
	ro.Mu.Unlock()
	fmt.Printf("Leader has been updated from %s to %s", oldURL, newURL)
}

func (ro *Router) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	ro.Mu.RLock()
	leader := ro.LeaderURL
	followers := make([]string, len(ro.FollowerURLs))
	copy(followers, ro.FollowerURLs)
	tokens := len(ro.Tokens)
	ro.Mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")

	response := struct {
		LeaderURL    string   `json:"leader_url"`
		FollowerURLs []string `json:"follower_urls"`
		TokenCount   int      `json:"token_count"`
	}{
		LeaderURL:    leader,
		FollowerURLs: followers,
		TokenCount:   tokens,
	}

	err := json.NewEncoder(w).Encode(response)
	if err != nil {
		fmt.Printf("failed to encode cluster status: %v\n", err)
	}
}

func (ro *Router) RegisterRoutes(Mux *http.ServeMux) {
	Mux.HandleFunc("PUT /keys/{key}", ro.handleWrite)
	Mux.HandleFunc("GET /keys/{key}", ro.handleRead)
	Mux.HandleFunc("DELETE /keys/{key}", ro.handleWrite)
	Mux.HandleFunc("GET /cluster/status", ro.handleClusterStatus)
}
