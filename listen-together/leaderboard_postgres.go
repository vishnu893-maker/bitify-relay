package main

// The Postgres side of the listening board (leaderboard/leaderboard.go).
//
// The same reason as the vault: a board that forgets its listeners on every
// release is not a board. One row per listener, the install ids that belong
// to them in a JSON list, so a person who signs in after playing signed out
// still has one row.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/KabirSinghBhatia/BitChord/backend/leaderboard"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const leaderboardPostgresSchema = `
CREATE TABLE IF NOT EXISTS app_leaderboard (
    id       TEXT PRIMARY KEY,
    name     TEXT NOT NULL DEFAULT '',
    ms       BIGINT NOT NULL DEFAULT 0,
    last_ms  BIGINT NOT NULL DEFAULT 0,
    installs JSONB NOT NULL DEFAULT '[]'::jsonb
);`

type leaderboardPostgres struct {
	db *sql.DB
}

func newLeaderboardPostgres(dsn string) (*leaderboardPostgres, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	if _, err := db.Exec(leaderboardPostgresSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("leaderboard schema: %w", err)
	}
	return &leaderboardPostgres{db: db}, nil
}

// LoadBoard reads the board back.
func (p *leaderboardPostgres) LoadBoard() ([]leaderboard.Row, error) {
	rows, err := p.db.Query(`SELECT id, name, ms, last_ms, installs FROM app_leaderboard`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var board []leaderboard.Row
	for rows.Next() {
		var row leaderboard.Row
		var installs []byte
		if err := rows.Scan(&row.ID, &row.Name, &row.Ms, &row.LastMs, &installs); err != nil {
			return nil, err
		}
		if len(installs) > 0 {
			_ = json.Unmarshal(installs, &row.Installs)
		}
		board = append(board, row)
	}
	return board, rows.Err()
}

// SaveBoard writes the whole board back: the rows that are there are
// updated, and the rows that are gone are removed — the board is a picture of
// who is listening, not a log of who once did.
func (p *leaderboardPostgres) SaveBoard(rows []leaderboard.Row) error {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		keep[row.ID] = true
		installs, err := json.Marshal(row.Installs)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO app_leaderboard (id, name, ms, last_ms, installs)
			VALUES ($1, $2, $3, $4, $5::jsonb)
			ON CONFLICT (id) DO UPDATE SET
				name     = EXCLUDED.name,
				ms       = EXCLUDED.ms,
				last_ms  = EXCLUDED.last_ms,
				installs = EXCLUDED.installs`,
			row.ID, row.Name, row.Ms, row.LastMs, string(installs)); err != nil {
			return err
		}
	}
	// The rows that are no longer on the board are gone from it.
	existing, err := tx.Query(`SELECT id FROM app_leaderboard`)
	if err != nil {
		return err
	}
	var stale []string
	for existing.Next() {
		var id string
		if err := existing.Scan(&id); err != nil {
			existing.Close()
			return err
		}
		if !keep[id] {
			stale = append(stale, id)
		}
	}
	existing.Close()
	if err := existing.Err(); err != nil {
		return err
	}
	for _, id := range stale {
		if _, err := tx.Exec(`DELETE FROM app_leaderboard WHERE id = $1`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
