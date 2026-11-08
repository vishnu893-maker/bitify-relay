package main

// The account store (2026-11-07): where an app login lives.
//
// Until now the only identity the server knew was whatever the YouTube Music
// cookie implied, hashed by the client. That made the vault work, but it left
// no way to answer "how many people use this" and no way to sign in on a new
// device from a fact only the server holds. This file adds that fact: a row per
// account, a row per session, and one stable vault id per account so the same
// login on a phone and a PC lands on the same streak and the same minutes.
//
// The interface is deliberately small and the implementation is deliberately
// split in two: Postgres is the real store (account_store_postgres.go), and an
// in-memory copy lives here so the whole flow can be exercised in a test or on
// a laptop without a database (JAM_AUTH_MEMORY=1). Handlers only ever see the
// interface.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// The three ways a create or a link can lose a race, told apart so the handler
// can say "that email is taken" instead of a generic failure.
var (
	errEmailTaken  = errors.New("email already registered")
	errGoogleTaken = errors.New("google account already registered")
	errVaultTaken  = errors.New("vault id already in use")
)

// AccountUser is one app account. PasswordHash is bcrypt and never leaves the
// server; GoogleSub is a linked Google account's stable subject id.
type AccountUser struct {
	ID           int64
	Email        string
	EmailNorm    string
	Name         string
	PasswordHash string
	GoogleSub    string
	VaultID      string
	CreatedAt    time.Time
	LastLoginAt  time.Time
	LoginCount   int64
}

// AccountSession is one signed-in device. Only the hash of the bearer token is
// stored, so a database read never yields a usable token.
type AccountSession struct {
	TokenHash  string
	UserID     int64
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// AccountStats is what the admin endpoint reports: the numbers the owner asked
// for ("kitne user hai, kitne login hue hai") and a little context beside them.
type AccountStats struct {
	TotalUsers     int64 `json:"totalUsers"`
	GoogleUsers    int64 `json:"googleUsers"`
	PasswordUsers  int64 `json:"passwordUsers"`
	TotalLogins    int64 `json:"totalLogins"`
	ActiveToday    int64 `json:"activeToday"`
	Active7d       int64 `json:"active7d"`
	NewToday       int64 `json:"newToday"`
	New7d          int64 `json:"new7d"`
	ActiveSessions int64 `json:"activeSessions"`
}

// CreateUserInput is one new account, already validated and hashed by the
// handler. VaultID is pre-generated; a collision is reported as errVaultTaken
// so the caller can try another.
type CreateUserInput struct {
	Email        string
	EmailNorm    string
	Name         string
	PasswordHash string
	GoogleSub    string
	VaultID      string
	Now          time.Time
}

// AccountStore is the whole surface the handlers use. Every method is safe for
// concurrent use.
type AccountStore interface {
	CreateUser(ctx context.Context, in CreateUserInput) (AccountUser, error)
	UserByEmail(ctx context.Context, emailNorm string) (AccountUser, bool, error)
	UserByGoogleSub(ctx context.Context, sub string) (AccountUser, bool, error)
	UserByID(ctx context.Context, id int64) (AccountUser, bool, error)
	LinkGoogle(ctx context.Context, id int64, sub string) error
	RecordLogin(ctx context.Context, id int64, at time.Time) error
	CreateSession(ctx context.Context, tokenHash string, userID int64, createdAt, expiresAt time.Time) error
	Session(ctx context.Context, tokenHash string) (AccountSession, bool, error)
	TouchSession(ctx context.Context, tokenHash string, at time.Time) error
	DeleteSession(ctx context.Context, tokenHash string) error
	Stats(ctx context.Context, now time.Time) (AccountStats, error)
}

// normalizeEmail is the one spelling of an address. Lowercasing and trimming is
// all this does: anything cleverer (dropping gmail dots, plus tags) would fold
// two real people into one account.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// memoryAccountStore keeps everything in RAM. It exists for tests and for a
// laptop run with JAM_AUTH_MEMORY=1; a restart loses it, which is exactly why
// the production path is Postgres.
type memoryAccountStore struct {
	mu       sync.Mutex
	nextID   int64
	users    map[int64]*AccountUser
	byEmail  map[string]int64
	byGoogle map[string]int64
	byVault  map[string]int64
	sessions map[string]AccountSession
}

func newMemoryAccountStore() *memoryAccountStore {
	return &memoryAccountStore{
		users:    map[int64]*AccountUser{},
		byEmail:  map[string]int64{},
		byGoogle: map[string]int64{},
		byVault:  map[string]int64{},
		sessions: map[string]AccountSession{},
	}
}

func (m *memoryAccountStore) CreateUser(_ context.Context, in CreateUserInput) (AccountUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byEmail[in.EmailNorm]; exists {
		return AccountUser{}, errEmailTaken
	}
	if _, exists := m.byVault[in.VaultID]; exists {
		return AccountUser{}, errVaultTaken
	}
	if in.GoogleSub != "" {
		if _, exists := m.byGoogle[in.GoogleSub]; exists {
			return AccountUser{}, errGoogleTaken
		}
	}
	m.nextID++
	user := AccountUser{
		ID:           m.nextID,
		Email:        in.Email,
		EmailNorm:    in.EmailNorm,
		Name:         in.Name,
		PasswordHash: in.PasswordHash,
		GoogleSub:    in.GoogleSub,
		VaultID:      in.VaultID,
		CreatedAt:    in.Now,
		LastLoginAt:  in.Now,
		LoginCount:   0,
	}
	stored := user
	m.users[user.ID] = &stored
	m.byEmail[in.EmailNorm] = user.ID
	m.byVault[in.VaultID] = user.ID
	if in.GoogleSub != "" {
		m.byGoogle[in.GoogleSub] = user.ID
	}
	return user, nil
}

func (m *memoryAccountStore) UserByEmail(_ context.Context, emailNorm string) (AccountUser, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[emailNorm]
	if !ok {
		return AccountUser{}, false, nil
	}
	return *m.users[id], true, nil
}

func (m *memoryAccountStore) UserByGoogleSub(_ context.Context, sub string) (AccountUser, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byGoogle[sub]
	if !ok {
		return AccountUser{}, false, nil
	}
	return *m.users[id], true, nil
}

func (m *memoryAccountStore) UserByID(_ context.Context, id int64) (AccountUser, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[id]
	if !ok {
		return AccountUser{}, false, nil
	}
	return *user, true, nil
}

func (m *memoryAccountStore) LinkGoogle(_ context.Context, id int64, sub string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[id]
	if !ok {
		return errors.New("user not found")
	}
	if other, taken := m.byGoogle[sub]; taken && other != id {
		return errGoogleTaken
	}
	user.GoogleSub = sub
	m.byGoogle[sub] = id
	return nil
}

func (m *memoryAccountStore) RecordLogin(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[id]
	if !ok {
		return errors.New("user not found")
	}
	user.LastLoginAt = at
	user.LoginCount++
	return nil
}

func (m *memoryAccountStore) CreateSession(_ context.Context, tokenHash string, userID int64, createdAt, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[tokenHash] = AccountSession{
		TokenHash:  tokenHash,
		UserID:     userID,
		CreatedAt:  createdAt,
		LastSeenAt: createdAt,
		ExpiresAt:  expiresAt,
	}
	return nil
}

func (m *memoryAccountStore) Session(_ context.Context, tokenHash string) (AccountSession, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[tokenHash]
	return session, ok, nil
}

func (m *memoryAccountStore) TouchSession(_ context.Context, tokenHash string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[tokenHash]
	if !ok {
		return nil
	}
	session.LastSeenAt = at
	m.sessions[tokenHash] = session
	return nil
}

func (m *memoryAccountStore) DeleteSession(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash)
	return nil
}

func (m *memoryAccountStore) Stats(_ context.Context, now time.Time) (AccountStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekAgo := now.Add(-7 * 24 * time.Hour)
	var stats AccountStats
	for _, user := range m.users {
		stats.TotalUsers++
		if user.GoogleSub != "" {
			stats.GoogleUsers++
		}
		if user.PasswordHash != "" {
			stats.PasswordUsers++
		}
		stats.TotalLogins += user.LoginCount
		if !user.CreatedAt.Before(startOfDay) {
			stats.NewToday++
		}
		if !user.CreatedAt.Before(weekAgo) {
			stats.New7d++
		}
		if !user.LastLoginAt.Before(startOfDay) {
			stats.ActiveToday++
		}
		if !user.LastLoginAt.Before(weekAgo) {
			stats.Active7d++
		}
	}
	for _, session := range m.sessions {
		if session.ExpiresAt.After(now) {
			stats.ActiveSessions++
		}
	}
	return stats, nil
}
