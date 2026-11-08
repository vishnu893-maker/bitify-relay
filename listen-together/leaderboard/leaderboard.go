// Package leaderboard keeps the all-time listening board: who has listened the
// longest, under the name they chose.
//
// The apps' presence heartbeat carries two optional fields — the nickname the
// user set and this install's all-time listening time in milliseconds. The
// store keeps, per install id, the latest name and the highest total ever
// reported (a total can only go up; an app restored from backup cannot pull
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
	maxMs        = int64(10*365*24*60*60) * 1000 // ten years; nothing real gets near it
)

type Entry struct {
	Name string `json:"name"`
	// Milliseconds listened, all time. The app formats it; the board ranks it.
	Ms int64 `json:"ms"`
}

type Snapshot struct {
	Leaders   []Entry `json:"leaders"`
	Listeners int     `json:"listeners"`
	UpdatedAt int64   `json:"updatedAt"`
}

type record struct {
	Name   string `json:"name,omitempty"`
	Ms     int64  `json:"ms"`
	LastMs int64  `json:"lastMs"`
	// Songs is the pre-2026-10 shape: a play count with no time. It is read
	// only so a saved board can drop those rows instead of publishing them as
	// a 0-minute listener.
	Songs *int `json:"songs,omitempty"`
}

// demoName reports the placeholder row that used to be seeded into a fresh
// board. It is not a person and must never take a place on the leaderboard.
func demoName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "test listener")
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
			dropped := 0
			for id, r := range saved {
				// A legacy row has no millisecond total to rank, and the demo
				// row is not a person. Drop both; the next heartbeat from a
				// real install puts it back with a time.
				if r == nil || r.Songs != nil || demoName(r.Name) {
					delete(saved, id)
					dropped++
					continue
				}
			}
			s.byID = saved
			log.Printf("leaderboard: %d listeners loaded from %s (%d stale rows dropped)",
				len(saved), filepath.Base(path), dropped)
		}
	} else if !os.IsNotExist(err) {
		log.Printf("leaderboard: could not read %s: %v", path, err)
	}
	return s
}

// Record folds one heartbeat in. A blank name keeps the entry (its count still
// totals) but keeps it off the board until the user picks a name.
func (s *Store) Record(idHex, name string, ms int64, nowMs int64) {
	if ms < 0 {
		ms = 0
	}
	if ms > maxMs {
		ms = maxMs
	}
	name = sanitizeName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if demoName(name) {
		// The old seed row may still be in memory from a pre-fix process.
		if _, ok := s.byID[idHex]; ok {
			delete(s.byID, idHex)
			s.dirty = true
		}
		return
	}
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
	if ms > r.Ms {
		r.Ms = ms
	}
	r.LastMs = nowMs
	s.dirty = true
}

// Snapshot returns the top [n] named listeners, longest-listened first, plus
// how many installs the board counts. Ties read first-come: the sort is stable
// over id order, which the map gives no promises about, but the numbers are
// what matters on a board. An install that never set a name counts toward the
// total but is not a row; a named install with 0 minutes is a row, so a new
// account can see itself on the board before its first full song.
func (s *Store) Snapshot(n int, nowMs int64) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]Entry, 0, len(s.byID))
	for _, r := range s.byID {
		if r.Name != "" {
			entries = append(entries, Entry{Name: r.Name, Ms: r.Ms})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Ms > entries[j].Ms })
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
