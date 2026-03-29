package wal

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

type WAL struct {
	file          *os.File
	currentOffset uint64
	mu            sync.Mutex
}
type Entry struct {
	Offset    uint64    `json:"offset"`
	Op        string    `json:"op"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Timestamp time.Time `json:"timestamp"`
}

func Open(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		count += 1
	}
	if err := scanner.Err(); err != nil {
		f.Close()
		return nil, err
	}
	w := WAL{file: f, currentOffset: uint64(count)}
	return &w, nil
}

func (w *WAL) Append(op, key, value string) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	assignedOffset := w.currentOffset
	e := Entry{Offset: assignedOffset, Op: op, Key: key, Value: value, Timestamp: time.Now()}
	data, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	_, err = w.file.Write(append(data, '\n'))
	if err != nil {
		return 0, err
	}
	err = w.file.Sync()
	if err != nil {
		return 0, err
	}
	w.currentOffset += 1
	return assignedOffset, nil
}

func (w *WAL) ReadAll() ([]Entry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.file.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	scanner := bufio.NewScanner(w.file)
	for scanner.Scan() {
		var e Entry
		err := json.Unmarshal(scanner.Bytes(), &e)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	err = scanner.Err()
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (w *WAL) ReadFrom(offset uint64) ([]Entry, error) {
	all_entries, err := w.ReadAll()
	if err != nil {
		return nil, err
	}
	var valid_entries []Entry
	for _, val := range all_entries {
		if val.Offset >= offset {
			valid_entries = append(valid_entries, val)
		}
	}
	return valid_entries, nil
}

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.file.Close()
	return err
}
