package main

// A one-time repair of the listening board (2026-11-27).
//
// The board predates the rule that only a real account may write a row. Before
// that rule an unauthenticated ping could mint a row under any 32-hex key and
// any name, so the live board grew rows — "✊👈" sat at the top — that belong
// to no account at all, next to real ones whose totals had been adopted from
// them.
//
// At boot, once the accounts are loaded, the board drops every row whose id is
// not a vault id the server issued. It is best-effort: a database error leaves
// the board alone, because a wrong prune is worse than a stale row. A row that
// belongs to a real account is never touched, and the account's own next ping
// puts it back if it somehow was.

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/vishnu893-maker/bitify-relay/listen-together/leaderboard"
)

// repairKnownBoardRows is a one-time cleanup of the rows the spoof left behind
// (2026-11-27). It is idempotent — once a score is zeroed or two rows are one,
// it does nothing — so it is safe to leave in place, and can be deleted once
// the live board is clean.
func repairKnownBoardRows(board *leaderboard.Store) {
	if board == nil {
		return
	}
	// The forged top row's score was not listening time (the name is a raised
	// fist and a pointing hand). Zero it; the board only ever raises a total,
	// so a genuine ping from that account would put its true minutes back.
	if n := board.SetMinutesMatching(func(name string) bool {
		return strings.ContainsRune(name, '\u270A') && strings.ContainsRune(name, '\U0001F448')
	}, 0); n > 0 {
		log.Printf("leaderboard repair: zeroed %d forged row(s)", n)
	}
	// One person signed in on two phones ended up as two accounts, so the
	// board showed the same minutes twice under two names. Fold them into one
	// row, keeping the larger total so the same listening is counted once.
	if kept, removed := board.MergeByName([]string{"developer", "vishnu"}); removed > 0 {
		log.Printf("leaderboard repair: merged %d row(s) into %q", removed, kept)
	}
}

func pruneOrphanBoardRows(board *leaderboard.Store) {
	if board == nil {
		return
	}
	pg, ok := accountStore.(*postgresAccountStore)
	if !ok || pg == nil {
		// No account database: the board is file-backed and its rows mix
		// account keys with install keys, and there is no table to check
		// against. Leave it be.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := pg.db.QueryContext(ctx, `SELECT vault_id FROM app_users`)
	if err != nil {
		log.Printf("leaderboard prune: could not read accounts (%v); leaving the board", err)
		return
	}
	defer rows.Close()
	real := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Printf("leaderboard prune: scan failed (%v); leaving the board", err)
			return
		}
		real[id] = true
	}
	if err := rows.Err(); err != nil {
		log.Printf("leaderboard prune: %v; leaving the board", err)
		return
	}
	var orphans []string
	for _, row := range board.Rows() {
		if row.ID != "" && !real[row.ID] {
			orphans = append(orphans, row.ID)
		}
	}
	if removed := board.RemoveAll(orphans); removed > 0 {
		log.Printf("leaderboard prune: dropped %d row(s) that belong to no account", removed)
	}
}
