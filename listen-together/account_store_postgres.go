package main

// The Postgres side of the account store (account_store.go) — the real one.
//
// Two small tables, one row per account and one per signed-in device. The
// schema is created on boot with IF NOT EXISTS, so a fresh Render service (or a
// fresh Neon database) needs no migration step: point DATABASE_URL at it and
// the first request works.
//
// Unique constraints do the concurrency work. Two devices registering the same
// email at once race in the database, and the loser gets a 23505 the driver
// hands back whole, so the handler can tell "email taken" from "vault id
// collided" from a genuine outage.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const accountSchema = `
CREATE TABLE IF NOT EXISTS app_users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL,
    email_norm    TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    google_sub    TEXT NOT NULL DEFAULT '',
    vault_id      TEXT NOT NULL UNIQUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    login_count   BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS app_users_google_sub_key
    ON app_users (google_sub) WHERE google_sub <> '';

CREATE TABLE IF NOT EXISTS app_sessions (
    token_hash   TEXT PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS app_sessions_user_idx ON app_sessions (user_id);
CREATE INDEX IF NOT EXISTS app_sessions_expires_idx ON app_sessions (expires_at);
`

type postgresAccountStore struct {
	db *sql.DB
}

// newPostgresAccountStore opens the pool and makes sure the schema is there. A
// failure here is not fatal to the server: the account endpoints answer 503 and
// everything else keeps working.
func newPostgresAccountStore(dsn string) (*postgresAccountStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// A small pool: Render Free runs one process, and the account endpoints are
	// a handful of queries per login.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, accountSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &postgresAccountStore{db: db}, nil
}

// constraintError turns a Postgres unique violation into the store's own
// sentinel, so handlers never import the driver.
func constraintError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "app_users_email_norm_key":
		return errEmailTaken
	case "app_users_vault_id_key":
		return errVaultTaken
	case "app_users_google_sub_key":
		return errGoogleTaken
	default:
		return err
	}
}

const accountUserColumns = `id, email, email_norm, name, password_hash, google_sub,
	vault_id, created_at, last_login_at, login_count`

func scanAccountUser(row interface{ Scan(...any) error }) (AccountUser, error) {
	var user AccountUser
	err := row.Scan(
		&user.ID, &user.Email, &user.EmailNorm, &user.Name, &user.PasswordHash,
		&user.GoogleSub, &user.VaultID, &user.CreatedAt, &user.LastLoginAt,
		&user.LoginCount,
	)
	return user, err
}

func (p *postgresAccountStore) CreateUser(ctx context.Context, in CreateUserInput) (AccountUser, error) {
	row := p.db.QueryRowContext(ctx, `
		INSERT INTO app_users
			(email, email_norm, name, password_hash, google_sub, vault_id, created_at, last_login_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		RETURNING `+accountUserColumns,
		in.Email, in.EmailNorm, in.Name, in.PasswordHash, in.GoogleSub, in.VaultID, in.Now,
	)
	user, err := scanAccountUser(row)
	if err != nil {
		return AccountUser{}, constraintError(err)
	}
	return user, nil
}

func (p *postgresAccountStore) UserByEmail(ctx context.Context, emailNorm string) (AccountUser, bool, error) {
	row := p.db.QueryRowContext(ctx, `SELECT `+accountUserColumns+` FROM app_users WHERE email_norm = $1`, emailNorm)
	user, err := scanAccountUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountUser{}, false, nil
	}
	if err != nil {
		return AccountUser{}, false, err
	}
	return user, true, nil
}

func (p *postgresAccountStore) UserByGoogleSub(ctx context.Context, sub string) (AccountUser, bool, error) {
	row := p.db.QueryRowContext(ctx, `SELECT `+accountUserColumns+` FROM app_users WHERE google_sub = $1`, sub)
	user, err := scanAccountUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountUser{}, false, nil
	}
	if err != nil {
		return AccountUser{}, false, err
	}
	return user, true, nil
}

func (p *postgresAccountStore) UserByID(ctx context.Context, id int64) (AccountUser, bool, error) {
	row := p.db.QueryRowContext(ctx, `SELECT `+accountUserColumns+` FROM app_users WHERE id = $1`, id)
	user, err := scanAccountUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountUser{}, false, nil
	}
	if err != nil {
		return AccountUser{}, false, err
	}
	return user, true, nil
}

func (p *postgresAccountStore) LinkGoogle(ctx context.Context, id int64, sub string) error {
	_, err := p.db.ExecContext(ctx, `UPDATE app_users SET google_sub = $2 WHERE id = $1`, id, sub)
	if err != nil {
		return constraintError(err)
	}
	return nil
}

func (p *postgresAccountStore) RecordLogin(ctx context.Context, id int64, at time.Time) error {
	_, err := p.db.ExecContext(ctx, `
		UPDATE app_users SET last_login_at = $2, login_count = login_count + 1
		WHERE id = $1`, id, at)
	return err
}

func (p *postgresAccountStore) CreateSession(ctx context.Context, tokenHash string, userID int64, createdAt, expiresAt time.Time) error {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO app_sessions (token_hash, user_id, created_at, last_seen_at, expires_at)
		VALUES ($1, $2, $3, $3, $4)`, tokenHash, userID, createdAt, expiresAt)
	return err
}

func (p *postgresAccountStore) Session(ctx context.Context, tokenHash string) (AccountSession, bool, error) {
	var session AccountSession
	err := p.db.QueryRowContext(ctx, `
		SELECT token_hash, user_id, created_at, last_seen_at, expires_at
		FROM app_sessions WHERE token_hash = $1`, tokenHash).Scan(
		&session.TokenHash, &session.UserID, &session.CreatedAt,
		&session.LastSeenAt, &session.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountSession{}, false, nil
	}
	if err != nil {
		return AccountSession{}, false, err
	}
	return session, true, nil
}

func (p *postgresAccountStore) TouchSession(ctx context.Context, tokenHash string, at time.Time) error {
	_, err := p.db.ExecContext(ctx, `UPDATE app_sessions SET last_seen_at = $2 WHERE token_hash = $1`, tokenHash, at)
	return err
}

func (p *postgresAccountStore) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM app_sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (p *postgresAccountStore) Stats(ctx context.Context, now time.Time) (AccountStats, error) {
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekAgo := now.Add(-7 * 24 * time.Hour)
	var stats AccountStats
	err := p.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE google_sub <> ''),
			COUNT(*) FILTER (WHERE password_hash <> ''),
			COALESCE(SUM(login_count), 0),
			COUNT(*) FILTER (WHERE last_login_at >= $1),
			COUNT(*) FILTER (WHERE last_login_at >= $2),
			COUNT(*) FILTER (WHERE created_at >= $1),
			COUNT(*) FILTER (WHERE created_at >= $2)
		FROM app_users`, startOfDay, weekAgo).Scan(
		&stats.TotalUsers, &stats.GoogleUsers, &stats.PasswordUsers,
		&stats.TotalLogins, &stats.ActiveToday, &stats.Active7d,
		&stats.NewToday, &stats.New7d,
	)
	if err != nil {
		return AccountStats{}, err
	}
	if err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM app_sessions WHERE expires_at > $1`, now).Scan(&stats.ActiveSessions); err != nil {
		return AccountStats{}, err
	}
	return stats, nil
}

// Compile-time proof the Postgres store satisfies the interface the handlers
// use, so a drift on either side is a build error rather than a runtime one.
var _ AccountStore = (*postgresAccountStore)(nil)
