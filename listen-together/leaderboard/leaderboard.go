// Package leaderboard keeps the all-time listening board: who has played the
// most songs, under the name they chose.
//
// The apps' presence heartbeat now carries two optional fields — the nickname
// the user set and the number of songs this install has played. The store
// keeps, per install id, the latest name and the highest song count ever
// reported (a count can only go up; an app restored from backup cannot pull
// the board backwards). Ids never leave the package: /api/stats/leaderboard
// publishes names and numbers only.
//
// The board survives a process restart through a small JSON file, flushed at
// most once every 30 s so a party-full of pings costs a handful of writes an
// hour. Render's disk is ephemeral across deploys — the board then starts
// again from whatever the apps re-report on their next ping, which is the
// whole truth anyway.
package leaderboard

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxNameRunes = 24
	maxSongs     = 10_000_000
)

type Entry struct {
	Name  string `json:"name"`
	Songs int64  `json:"songs"`
}

type Snapshot struct {
	Leaders   []Entry `json:"leaders"`
	Listeners int     `json:"listeners"`
	UpdatedAt int64   `json:"updatedAt"`
}

type record struct {
	Name   string `json:"name,omitempty"`
	Songs  int64  `json:"songs"`
	LastMs int64  `json:"lastMs"`
}

type Store struct {
	mu       sync.Mutex
	byID     map[string]*record
	path     string
	dirty    bool
	flushing bool
}

// NewStore loads the board from [path] (a missing file is a fresh board).
func NewStore(path string) *Store {
	s := &Store{byID: make(map[string]*record), path: path}
	if data, err := os.ReadFile(path); err == nil {
		var saved map[string]*record
		if json.Unmarshal(data, &saved) == nil && len(saved) > 0 {
			s.byID = saved
			log.Printf("leaderboard: %d listeners loaded from %s", len(saved), filepath.Base(path))
		}
	} else if !os.IsNotExist(err) {
		log.Printf("leaderboard: could not read %s: %v", path, err)
	}
	return s
}

// Record folds one heartbeat in. A blank name keeps the entry (its count still
// totals) but keeps it off the board until the user picks a name.
func (s *Store) Record(idHex, name string, songs int64, nowMs int64) {
	if songs < 0 {
		songs = 0
	}
	if songs > maxSongs {
		songs = maxSongs
	}
	name = sanitizeName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byID[idHex]
	if r == nil {
		r = &record{}
		s.byID[idHex] = r
	}
	if name != "" || r.Name == "" {
		// An explicitly blank ping never erases a chosen name; a new one replaces it.
		if name != "" {
			r.Name = name
		}
	}
	if songs > r.Songs {
		r.Songs = songs
	}
	r.LastMs = nowMs
	s.dirty = true
}

// Snapshot returns the top [n] named listeners, most songs first, plus how
// many installs and songs the board totals. Ties read first-come: the sort is
// stable over id order, which the map gives no promises about, but the numbers
// are what matters on a board.
func (s *Store) Snapshot(n int, nowMs int64) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]Entry, 0, len(s.byID))
	total := int64(0)
	for _, r := range s.byID {
		total += r.Songs
		if r.Name != "" && r.Songs > 0 {
			entries = append(entries, Entry{Name: r.Name, Songs: r.Songs})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Songs > entries[j].Songs })
	if n > 0 && len(entries) > n {
		entries = entries[:n]
	}
	return Snapshot{Leaders: entries, Listeners: len(s.byID), UpdatedAt: nowMs}
}

// Flush writes the board out if it changed. Safe to call often; a no-op when clean.
func (s *Store) Flush() {
	s.mu.Lock()
	if !s.dirty || s.flushing {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	s.flushing = true
	data, err := json.Marshal(s.byID)
	s.mu.Unlock()
	if err == nil {
		tmp := s.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	s.mu.Lock()
	s.flushing = false
	if err != nil {
		// A record may have landed while the write was out; try again next tick.
		log.Printf("leaderboard: save failed: %v", err)
		s.dirty = true
	}
	s.mu.Unlock()
}

// StartFlushing saves changed boards every [interval] until the process ends.
func (s *Store) StartFlushing(interval time.Duration) {
	go func() {
		for range time.Tick(interval) {
			s.Flush()
		}
	}()
}

// sanitizeName keeps a name printable, short, and single-lined.
func sanitizeName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name))
	runes := []rune(name)
	if len(runes) > maxNameRunes {
		runes = runes[:maxNameRunes]
	}
	return strings.TrimSpace(string(runes))
}
