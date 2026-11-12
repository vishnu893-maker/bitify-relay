package main

// The Postgres side of the listening vault (vault.go) — the real one.
//
// The vault used to live in a JSON file beside the server. Render's disk is
// ephemeral across deploys, so a release could take a listener's play minutes
// with it: the file survives a restart, not a release. The database does not
// have that problem, so the vault lives there now and the file is only the
// fallback for a laptop run without DATABASE_URL.
//
// One row per account: the vault id, the cutover date (the first day whose
// minutes add across devices), and the devices' own buckets as one JSON
// document. The schema is created on boot with IF NOT EXISTS, so a fresh
// database needs no migration step — point DATABASE_URL at it and the first
// request works.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const vaultPostgresSchema = `
CREATE TABLE IF NOT EXISTS app_vaults (
    vault_id   TEXT PRIMARY KEY,
    updated_at BIGINT NOT NULL DEFAULT 0,
    cutover    TEXT NOT NULL DEFAULT '',
    devices    JSONB NOT NULL DEFAULT '{}'::jsonb
);`

type postgresVaultStore struct {
	db *sql.DB
}

func newPostgresVaultStore(dsn string) (*postgresVaultStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// A vault write is one small row, so a handful of connections is plenty.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	if _, err := db.Exec(vaultPostgresSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("vault schema: %w", err)
	}
	return &postgresVaultStore{db: db}, nil
}

// Load reads every account back.
func (p *postgresVaultStore) Load() (map[string]vaultAccount, error) {
	rows, err := p.db.Query(`SELECT vault_id, updated_at, cutover, devices FROM app_vaults`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := make(map[string]vaultAccount)
	for rows.Next() {
		var id, cutover string
		var updated int64
		var devices []byte
		if err := rows.Scan(&id, &updated, &cutover, &devices); err != nil {
			return nil, err
		}
		devs := map[string][]vaultBucket{}
		if len(devices) > 0 {
			if err := json.Unmarshal(devices, &devs); err != nil {
				log.Printf("vault: account %s has unreadable devices: %v", id, err)
				continue
			}
		}
		accounts[id] = vaultAccount{UpdatedAt: updated, Cutover: cutover, Devices: devs}
	}
	return accounts, rows.Err()
}

// Save writes one account back.
func (p *postgresVaultStore) Save(id string, account vaultAccount) error {
	devices, err := json.Marshal(account.Devices)
	if err != nil {
		return err
	}
	_, err = p.db.Exec(`INSERT INTO app_vaults (vault_id, updated_at, cutover, devices)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (vault_id) DO UPDATE SET
			updated_at = EXCLUDED.updated_at,
			cutover    = EXCLUDED.cutover,
			devices    = EXCLUDED.devices`,
		id, account.UpdatedAt, account.Cutover, string(devices))
	return err
}
