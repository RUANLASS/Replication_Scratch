package leader

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"wal_scratch/wal"
)

type Server struct {
	store map[string]string
	wal   *wal.WAL
	mu    sync.RWMutex
}

type JSONRequest struct {
	Value string `json:"value"`
}

func NewServer(walPath string) (*Server, error) {
	w, err := wal.Open(walPath)
	if err != nil {
		return nil, err
	}
	s := &Server{store: make(map[string]string), wal: w}
	err = s.replay()
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) replay() error {
	var ents []wal.Entry
	ents, err := s.wal.ReadAll()
	if err != nil {
		return err
	}

	for _, val := range ents {
		if val.Op == "set" {
			s.store[val.Key] = val.Value
		}
		if val.Op == "delete" {
			delete(s.store, val.Key)
		}
	}
	return nil
}

func (s *Server) handleSet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var body struct {
		Value string `json:"value"`
	}
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wal.Append("set", key, body.Value)
	s.store[key] = body.Value
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	s.mu.RLock()
	val, ok := s.store[key]
	s.mu.RUnlock()
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

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wal.Append("delete", key, " ")
	delete(s.store, key)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleWAL(w http.ResponseWriter, r *http.Request) {
	StrVar := r.URL.Query().Get("from")
	offset, err := strconv.ParseUint(StrVar, 10, 64)
	if err != nil {
		offset = 0
	}
	entries, err := s.wal.ReadFrom(offset)
	if err != nil {
		http.Error(w, "Failed to read WAL", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if entries == nil {
		entries = []wal.Entry{}
	}
	json.NewEncoder(w).Encode(entries)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := map[string]string{"status": "ok"}
	w.WriteHeader(http.StatusOK)
	err := json.NewEncoder(w).Encode(response)
	if err != nil {
		fmt.Printf("health check encoding error: %v\n", err)
	}
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /keys/{key}", s.handleSet)
	mux.HandleFunc("GET /keys/{key}", s.handleGet)
	mux.HandleFunc("DELETE /keys/{key}", s.handleDelete)
	mux.HandleFunc("GET /wal", s.handleWAL)
	mux.HandleFunc("GET /health", s.handleHealth)
}
