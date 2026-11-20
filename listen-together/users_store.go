package main

// The total-users tally (2026-11-20): every distinct identity the app has ever
// seen, counted once.
//
// The account store answers "how many accounts are there"; this answers "how
// many people have used Bitify", which is a different question: somebody who
// has only ever used the app signed out is a person too, and until now the
// server had no record of them at all.
//
// An identity is one string in one table, and the string says which kind it is:
//
//	install:<uuid>       a guest installation, from the id the app keeps
//	                     across restarts (AndroidPresence.installId)
//	account:<vaultId>    a registered account, from the vault id the server
//	                     itself issued (never a client-supplied id)
//
// The two kinds are deliberately separate rows, never merged: a guest who
// signs in is still one person who used the app, and the account they signed
// into is another identity. That is the rule the feature was specified with —
// a previously counted guest install is not replaced when it transitions to an
// account. The one place they fold together is idempotency: registering an
// install twice, or an account twice, changes nothing.
//
// Counting is a pure insert. The table's PRIMARY KEY is the whole concurrency
// story: INSERT ... ON CONFLICT DO NOTHING makes a registration atomic and
// idempotent at once, so a burst of duplicate requests — or two devices racing
// to register the same account — leaves exactly one row. The total is COUNT(*).
//
// The interface is small and the implementation split in two, exactly like the
// account store: Postgres is the real one (users_store_postgres.go) and an
// in-memory copy lives here so the flow can be tested, or run on a laptop with
// JAM_AUTH_MEMORY=1, without a database. Handlers only ever see the interface.

import (
	"context"
	"strings"
	"sync"
	"time"
)

// The two identity namespaces, and the kind each is stored under. Kept as
// prefixes on one column rather than two tables so the total is a single
// COUNT(*) and a future kind costs no schema change.
const (
	identityInstallPrefix = "install:"
	identityAccountPrefix = "account:"
	identityInstallKind   = "install"
	identityAccountKind   = "account"
)

// UserCounterStore is the whole surface the handlers use. Every method is safe
// for concurrent use and every insert is idempotent: calling it again with the
// same identity is a no-op, never a second row.
type UserCounterStore interface {
	// CountInstall records a guest installation, once.
	CountInstall(ctx context.Context, installID string, now time.Time) error
	// CountAccount records a registered account, once. vaultID is the value
	// the server issued at registration; it is never taken from the client.
	CountAccount(ctx context.Context, vaultID string, now time.Time) error
	// Breakdown is the tally split by kind: how many accounts and how many
	// guest installations. Their sum is the total, read from one snapshot so
	// the three numbers can never disagree.
	Breakdown(ctx context.Context) (accounts, installs int64, err error)
}

// installIdentity and accountIdentity are the one place the key format is
// written, so the prefix and the value can never drift apart.
func installIdentity(installID string) string { return identityInstallPrefix + installID }
func accountIdentity(vaultID string) string   { return identityAccountPrefix + vaultID }

// memoryUserCounterStore keeps the identities in RAM. It exists for tests and
// for a laptop run with JAM_AUTH_MEMORY=1; a restart loses it, which is exactly
// why the production path is Postgres.
type memoryUserCounterStore struct {
	mu         sync.Mutex
	identities map[string]struct{}
}

func newMemoryUserCounterStore() *memoryUserCounterStore {
	return &memoryUserCounterStore{identities: map[string]struct{}{}}
}

func (m *memoryUserCounterStore) CountInstall(_ context.Context, installID string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.identities[installIdentity(installID)] = struct{}{}
	return nil
}

func (m *memoryUserCounterStore) CountAccount(_ context.Context, vaultID string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.identities[accountIdentity(vaultID)] = struct{}{}
	return nil
}

func (m *memoryUserCounterStore) Breakdown(_ context.Context) (int64, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var accounts, installs int64
	for id := range m.identities {
		if strings.HasPrefix(id, identityAccountPrefix) {
			accounts++
		} else {
			installs++
		}
	}
	return accounts, installs, nil
}

// Compile-time proof the memory store satisfies the interface the handlers use.
var _ UserCounterStore = (*memoryUserCounterStore)(nil)
