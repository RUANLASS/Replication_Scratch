package follower

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"wal_scratch/wal"
)

type Follower struct {
	Store         map[string]string
	Wal           *wal.WAL
	Mu            sync.RWMutex
	LeaderURL     string
	AppliedOffset uint64
	Cancel        context.CancelFunc
	Promoted      int32
}

func NewFollower(walPath, leaderURL string) (*Follower, error) {
	wal_loc, err := wal.Open(walPath)
	if err != nil {
		return nil, err
	}
	follower := &Follower{
		Wal:       wal_loc,
		LeaderURL: leaderURL,
		Store:     make(map[string]string),
	}
	entries, err := wal_loc.ReadAll()
	if err != nil {
		wal_loc.Close()
		return nil, err
	}

	for _, entry := range entries {
		switch entry.Op {
		case "set":
			follower.Store[entry.Key] = entry.Value
		case "delete":
			delete(follower.Store, entry.Key)
		}
		follower.AppliedOffset = entry.Offset
	}
	return follower, nil
}

func (f *Follower) replay() error {
	var ents []wal.Entry
	ents, err := f.Wal.ReadAll()
	if err != nil {
		return err
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	for _, val := range ents {
		if val.Op == "set" {
			f.Store[val.Key] = val.Value
		}
		if val.Op == "delete" {
			delete(f.Store, val.Key)
		}
		f.AppliedOffset = val.Offset
	}
	return nil
}

func (f *Follower) Start(interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())

	f.Mu.Lock()
	f.Cancel = cancel
	f.Mu.Unlock()

	ticker := time.NewTicker(interval)

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := f.Poll()
				if err != nil {
					fmt.Printf("Poll error: %v\n", err)
				}
			}
		}
	}()
}

func (f *Follower) Poll() error {
	f.Mu.RLock()
	nextOffset := uint64(0)
	if len(f.Store) > 0 || f.AppliedOffset > 0 {
		nextOffset = f.AppliedOffset + 1
	}
	f.Mu.RUnlock()
	url := fmt.Sprintf("%s/wal?from=%d", f.LeaderURL, nextOffset)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("leader returned error: %d", resp.StatusCode)
	}
	var body []wal.Entry
	err = json.NewDecoder(resp.Body).Decode(&body)
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	if len(body) == 0 {
		return nil
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	for _, ent := range body {
		_, err = f.Wal.Append(ent.Op, ent.Key, ent.Value)
		if err != nil {
			return err
		}
		switch ent.Op {
		case "set":
			f.Store[ent.Key] = ent.Value
		case "delete":
			delete(f.Store, ent.Key)
		}
		f.AppliedOffset = ent.Offset
	}
	return nil
}

func (f *Follower) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	f.Mu.RLock()
	val, ok := f.Store[key]
	f.Mu.RUnlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "key not found",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	response := map[string]string{
		"key":   key,
		"value": val,
	}

	err := json.NewEncoder(w).Encode(response)
	if err != nil {
		fmt.Printf("failed to encode response: %v\n", err)
	}
}

func (f *Follower) handleStatus(w http.ResponseWriter, r *http.Request) {
	f.Mu.RLock()
	defer f.Mu.RUnlock()
	response := struct {
		AppliedOffset uint64 `json:"applied_offset"`
		LeaderURL     string `json:"leader_url"`
	}{
		AppliedOffset: f.AppliedOffset,
		LeaderURL:     f.LeaderURL,
	}
	err := json.NewEncoder(w).Encode(response)
	if err != nil {
		fmt.Printf("failed to encode response: %v\n", err)
	}
}

func (f *Follower) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := map[string]string{"status": "ok"}
	w.WriteHeader(http.StatusOK)
	err := json.NewEncoder(w).Encode(response)
	if err != nil {
		fmt.Printf("health check encoding error: %v\n", err)
	}
}

func (f *Follower) handlePut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	f.Mu.Lock()
	defer f.Mu.Unlock()

	offset, err := f.Wal.Append("set", key, body.Value)
	if err != nil {
		http.Error(w, "failed to log write", http.StatusInternalServerError)
		return
	}

	f.Store[key] = body.Value
	f.AppliedOffset = offset

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"key":    key,
		"value":  body.Value,
		"offset": offset,
	})
}

func (f *Follower) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	f.Mu.Lock()
	defer f.Mu.Unlock()

	offset, err := f.Wal.Append("delete", key, "")
	if err != nil {
		http.Error(w, "failed to log delete", http.StatusInternalServerError)
		return
	}

	delete(f.Store, key)
	f.AppliedOffset = offset

	w.WriteHeader(http.StatusNoContent)
}

func (f *Follower) handleSwitch(w http.ResponseWriter, r *http.Request) {
	// Check the atomic variable we set in Promote()
	if !f.IsPromoted() {
		w.Header().Set("Allow", "GET")
		http.Error(w, "In Follower mode: writes not allowed", http.StatusMethodNotAllowed)
		return
	}

	// If promoted, forward to the actual write logic
	switch r.Method {
	case http.MethodPut:
		f.handlePut(w, r)
	case http.MethodDelete:
		f.handleDelete(w, r)
	}
}

func (f *Follower) Promote() {
	atomic.StoreInt32(&f.Promoted, 1)
	if f.Cancel != nil {
		f.Cancel()
	}
}

func (f *Follower) IsPromoted() bool {
	return atomic.LoadInt32(&f.Promoted) == 1
}

func (f *Follower) handlePromote(w http.ResponseWriter, r *http.Request) {
	f.Promote()
	w.WriteHeader(http.StatusOK)

}

func (f *Follower) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /keys/{key}", f.handleGet)
	mux.HandleFunc("GET /status", f.handleStatus)
	mux.HandleFunc("GET /health", f.handleHealth)
	mux.HandleFunc("POST /promote", f.handlePromote)
	if f.IsPromoted() {
		mux.HandleFunc("PUT /keys/{key}", f.handleSwitch)
		mux.HandleFunc("DELETE /keys/{key}", f.handleSwitch)
	}
}

func FetchAppliedOffset(followerURL string) (uint64, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(followerURL + "/status")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("follower %s returned status %d", followerURL, resp.StatusCode)
	}
	var status struct {
		AppliedOffset uint64 `json:"applied_offset"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return 0, fmt.Errorf("failed to decode status from %s: %w", followerURL, err)
	}

	return status.AppliedOffset, nil
}

func (f *Follower) Close() error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Cancel != nil {
		f.Cancel()
	}
	if f.Wal != nil {
		return f.Wal.Close()
	}
	return nil

}
