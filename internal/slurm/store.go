package slurm

import "sync"

type Store struct {
	mu   sync.RWMutex
	data map[string]Snapshot
}

func NewStore() *Store { return &Store{data: make(map[string]Snapshot)} }

func (s *Store) Set(id string, snapshot Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[id] = snapshot
}

func (s *Store) Get(id string) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.data[id]
	return snapshot, ok
}

func (s *Store) All() map[string]Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Snapshot, len(s.data))
	for id, snapshot := range s.data {
		out[id] = snapshot
	}
	return out
}
