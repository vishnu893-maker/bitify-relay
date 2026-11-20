package main

// The Postgres side of the total-users tally (users_store.go).
//
// One table, one row per identity, created on boot with IF NOT EXISTS so a
// fresh Render service (or a fresh Neon database) needs no migration step.
//
// The PRIMARY KEY on identity is the concurrency work: every registration is an
// INSERT ... ON CONFLICT DO NOTHING, which is atomic and idempotent in one
// statement. Two devices registering the same account at once — or the same
// install retrying after a timeout — leave exactly one row, and none of them
// can inflate the count by repeating the call.
//
// On boot the accounts that already exist are folded in once, so the tally
// starts from the people who were here before this feature rather than from
// zero. That backfill is best-effort: the table this feature owns is created
// regardless, and a database that is up but missing app_users (an older
// deploy) simply starts its account tally from the next sign-in.

import (
	"context"
	"database/sql"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const userCounterSchema = `
CREATE TABLE IF NOT EXISTS app_user_identities (
    identity   TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS app_user_identities_kind_idx ON app_user_identities (kind);`

// backfillAccountIdentities moves every existing account into the tally once.
// ON CONFLICT DO NOTHING makes it safe to run on every boot.
const backfillAccountIdentities = `
INSERT INTO app_user_identities (identity, kind)
SELECT 'account:' || vault_id, 'account' FROM app_users
ON CONFLICT (identity) DO NOTHING;`

type postgresUserCounterStore struct {
	db *sql.DB
}

// newPostgresUserCounterStore opens the pool, makes sure the schema is there,
// and folds in the accounts that already exist. It runs after the account store
// has created app_users, so the backfill has a table to read; a failure there
// is logged, not returned, because the tally this feature owns is already
// ready by then.
func newPostgresUserCounterStore(dsn string) (*postgresUserCounterStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, userCounterSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &postgresUserCounterStore{db: db}
	if _, err := db.ExecContext(ctx, backfillAccountIdentities); err != nil {
		log.Printf("users: could not fold in existing accounts (%v); the tally starts from new identities", err)
	}
	return store, nil
}

func (p *postgresUserCounterStore) CountInstall(ctx context.Context, installID string, now time.Time) error {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO app_user_identities (identity, kind, created_at)
		VALUES ($1, 'install', $2)
		ON CONFLICT (identity) DO NOTHING`, installIdentity(installID), now)
	return err
}

func (p *postgresUserCounterStore) CountAccount(ctx context.Context, vaultID string, now time.Time) error {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO app_user_identities (identity, kind, created_at)
		VALUES ($1, 'account', $2)
		ON CONFLICT (identity) DO NOTHING`, accountIdentity(vaultID), now)
	return err
}

func (p *postgresUserCounterStore) Total(ctx context.Context) (int64, error) {
	var total int64
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_user_identities`).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// Compile-time proof the Postgres store satisfies the interface the handlers
// use, so a drift on either side is a build error rather than a runtime one.
var _ UserCounterStore = (*postgresUserCounterStore)(nil)
