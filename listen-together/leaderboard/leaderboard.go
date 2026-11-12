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
// hour. On a server that has a database the board lives there instead (the
// Sink below), because Render's disk is ephemeral across deploys and a board
// that forgets its listeners on every release is not a board.
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
	// Installs are the per-device install ids that have reported under this
	// account. They are kept so a later sign-out (or a ping from an older
	// build that sends no account key) still lands on the person's own row
	// instead of splitting the board in two again.
	Installs []string `json:"installs,omitempty"`
}

// demoName reports the rows that are not people and must never take a place on
// the leaderboard: the placeholder the old server seeded, and the name the
// sync probes signed themselves in with.
func demoName(name string) bool {
	trimmed := strings.TrimSpace(name)
	return strings.EqualFold(trimmed, "test listener") ||
		strings.EqualFold(trimmed, "syncprobe")
}

type Store struct {
	mu       sync.Mutex
	byID     map[string]*record
	path     string
	sink     Sink
	dirty    bool
	flushing bool
}

// Row is one listener's row in the shape a durable store keeps it: the id the
// board files it under, the name it shows, the listening time it has, and the
// installs that belong to it.
type Row struct {
	ID       string
	Name     string
	Ms       int64
	LastMs   int64
	Installs []string
}

// Sink is a durable place for the board. The server has a database; a laptop
// run does not, and falls back to the board's own file.
type Sink interface {
	SaveBoard(rows []Row) error
}

// SetSink hands the board a durable store. It is called once, at boot.
func (s *Store) SetSink(sink Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// Rows returns every row the board holds, for a durable store to write.
func (s *Store) Rows() []Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rowsLocked()
}

func (s *Store) rowsLocked() []Row {
	rows := make([]Row, 0, len(s.byID))
	for id, r := range s.byID {
		if r == nil {
			continue
		}
		rows = append(rows, Row{
			ID:       id,
			Name:     r.Name,
			Ms:       r.Ms,
			LastMs:   r.LastMs,
			Installs: append([]string(nil), r.Installs...),
		})
	}
	return rows
}

// ReplaceRows loads rows into the board, the way a restart from a durable
// store does. A row with no name still counts as a listener.
func (s *Store) ReplaceRows(rows []Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID = make(map[string]*record, len(rows))
	for _, row := range rows {
		if row.ID == "" || demoName(row.Name) {
			continue
		}
		s.byID[row.ID] = &record{
			Name:     row.Name,
			Ms:       row.Ms,
			LastMs:   row.LastMs,
			Installs: append([]string(nil), row.Installs...),
		}
	}
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
	// An install that has reported under an account before still belongs to
	// that account's row, even when this ping carries no account key.
	idHex = s.resolveLocked(idHex)
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

// RecordAccount folds one heartbeat in under the account's own key, and folds
// the install's earlier row into it when there is one.
//
// The account key (the vault id) and the install id (a random per-device uuid)
// are two names for one person, and a client that had not signed in yet
// reported under the install id. When it signs in, its old row has to go: the
// board is a list of people, and one person with a phone and a PC is one row,
// not two rows that happen to share a face. The install row's minutes are
// folded in by the larger total, never by the sum — both rows report the same
// account's listening, so adding them would count an evening twice.
func (s *Store) RecordAccount(accountKey, installKey, name string, ms int64, nowMs int64) {
	s.mu.Lock()
	// The install may have reported under another key before. That happens
	// every time someone plays signed out (the key is then the hash of their
	// Google login) and later signs in (the key is then the account id the
	// server issued). The board files a person under one row, so the old
	// row has to go in with the new one — otherwise the same listener shows
	// up twice under two names, which is what a board must never do.
	if installKey != "" && installKey != accountKey {
		owner := s.resolveLocked(installKey)
		if owner != "" && owner != accountKey && owner != installKey {
			s.foldIntoLocked(owner, accountKey)
		}
	}
	if installKey != "" && installKey != accountKey {
		old := s.byID[installKey]
		if old != nil {
			s.foldIntoLocked(installKey, accountKey)
		}
		row := s.byID[accountKey]
		if row == nil {
			row = &record{}
			s.byID[accountKey] = row
		}
		if len(row.Installs) == 0 || row.Installs[len(row.Installs)-1] != installKey {
			row.Installs = appendInstall(row.Installs, installKey)
		}
	}
	s.mu.Unlock()
	s.Record(accountKey, name, ms, nowMs)
}

// foldIntoLocked moves a row into another one and drops it: the same listener
// under two keys is one row. The larger total wins (both rows report the same
// account's listening, so adding them would count an evening twice), the
// target's name is kept when it has one, and the install lists are joined.
func (s *Store) foldIntoLocked(from, to string) {
	src := s.byID[from]
	if src == nil {
		return
	}
	dst := s.byID[to]
	if dst == nil {
		dst = &record{}
		s.byID[to] = dst
	}
	if dst.Name == "" {
		dst.Name = src.Name
	}
	if src.Ms > dst.Ms {
		dst.Ms = src.Ms
	}
	if src.LastMs > dst.LastMs {
		dst.LastMs = src.LastMs
	}
	for _, install := range src.Installs {
		dst.Installs = appendInstall(dst.Installs, install)
	}
	delete(s.byID, from)
	s.dirty = true
}

// resolveLocked answers the row a key belongs to: an install that has already
// reported under an account belongs to that account's row.
func (s *Store) resolveLocked(key string) string {
	if key == "" {
		return key
	}
	for id, r := range s.byID {
		if r == nil {
			continue
		}
		for _, install := range r.Installs {
			if install == key {
				return id
			}
		}
	}
	return key
}

// appendInstall adds an install id to a row's list, once.
func appendInstall(list []string, install string) []string {
	for _, existing := range list {
		if existing == install {
			return list
		}
	}
	return append(list, install)
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
//
// With a durable store the board goes there; without one it goes to the file
// beside the server, which is what a laptop run uses.
func (s *Store) Flush() {
	s.mu.Lock()
	if !s.dirty || s.flushing {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	s.flushing = true
	if s.sink != nil {
		rows := s.rowsLocked()
		sink := s.sink
		s.mu.Unlock()
		err := sink.SaveBoard(rows)
		s.mu.Lock()
		s.flushing = false
		if err != nil {
			// A record may have landed while the write was out; try again next tick.
			log.Printf("leaderboard: save failed: %v", err)
			s.dirty = true
		}
		s.mu.Unlock()
		return
	}
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
