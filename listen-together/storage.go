package main

// Where the server keeps what it must not forget (storage.go).
//
// Two things on this server are people's, not the server's: the listening
// minutes of every account (vault.go) and the listening board
// (leaderboard/leaderboard.go). Both used to live in JSON files beside the
// binary. Render's disk is ephemeral across deploys — the files survive a
// restart, not a release — so a listener's minutes could vanish with a
// deploy and a board could forget its listeners.
//
// The accounts already live in the database (account_store_postgres.go), so
// the minutes and the board go there too. Without DATABASE_URL both keep to
// their files, which is what a laptop run without a database uses.

import (
	"log"

	"github.com/KabirSinghBhatia/BitChord/backend/config"
)

// storageInit gives the vault and the board their home. It is called once, at
// boot, before anything reads them.
func storageInit() {
	if config.DatabaseURL == "" {
		log.Printf("storage: no DATABASE_URL; the vault and the board live in files")
		return
	}
	if store, err := newPostgresVaultStore(config.DatabaseURL); err != nil {
		log.Printf("storage: the vault's database is unavailable (%v); the vault keeps its file", err)
	} else {
		vaultSetStore(store)
		log.Printf("storage: the vault lives in the database")
	}
	board, err := newLeaderboardPostgres(config.DatabaseURL)
	if err != nil {
		log.Printf("storage: the board's database is unavailable (%v); the board keeps its file", err)
		return
	}
	// A board that is already in the database wins: the file is only the
	// fallback, and a first run has to move the file's board over rather than
	// overwrite it with nothing.
	rows, err := board.LoadBoard()
	if err != nil {
		log.Printf("storage: the board could not be read from the database (%v); the board keeps its file", err)
		return
	}
	if len(rows) > 0 {
		leaderboardStore.ReplaceRows(rows)
		log.Printf("storage: %d listeners loaded from the database", len(rows))
	} else {
		log.Printf("storage: the database has no board yet; the file's board is kept and will be written there")
	}
	leaderboardStore.SetSink(board)
	log.Printf("storage: the board lives in the database")
}
