package auth

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// SessionTTL is fixed from login, not extended by use. The expiry is kept on
// the server, so a copied cookie stops working on time whatever the browser
// was told.
const SessionTTL = 7 * 24 * time.Hour

// Sessions lives in memory. A restart logs everyone out, which for a single
// owner is a smaller cost than a session file on disk.
type Sessions struct {
	mu  sync.Mutex
	m   map[string]time.Time
	ttl time.Duration
	now func() time.Time
}

func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{m: map[string]time.Time{}, ttl: ttl, now: time.Now}
}

// Create returns a new random session id.
func (s *Sessions) Create() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, exp := range s.m {
		if !now.Before(exp) {
			delete(s.m, k)
		}
	}
	s.m[id] = now.Add(s.ttl)
	return id, nil
}

func (s *Sessions) Valid(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[id]
	if ok && !s.now().Before(exp) {
		delete(s.m, id)
		return false
	}
	return ok
}

func (s *Sessions) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// DeleteOthers ends every session but keep, after a password change.
func (s *Sessions) DeleteOthers(keep string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.m {
		if id != keep {
			delete(s.m, id)
		}
	}
}
