package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// msgDB is the Postgres pool shared with the account store. Messaging needs a
// real database; with the in-memory account store the endpoints answer 503.
var msgDB *sql.DB

func messagesInit() {
	pg, ok := accountStore.(*postgresAccountStore)
	if !ok || pg == nil {
		log.Printf("messages: no postgres; messaging endpoints disabled")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pg.db.ExecContext(ctx, messagesSchema); err != nil {
		log.Printf("messages: schema failed (%v); messaging disabled", err)
		return
	}
	msgDB = pg.db
	log.Printf("messages: ready")
}

// msgAuth resolves the caller or writes the error. Every messaging route goes
// through here, so none is reachable without a live session.
func msgAuth(w http.ResponseWriter, r *http.Request) (AccountUser, bool) {
	if msgDB == nil {
		jsonError(w, http.StatusServiceUnavailable, "messages_disabled", "Messaging is not configured on this server.")
		return AccountUser{}, false
	}
	user, _, ok := currentUser(r.Context(), r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "Sign in to use Messages.")
		return AccountUser{}, false
	}
	return user, true
}

func readBody(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		jsonError(w, http.StatusBadRequest, "bad_request", "Invalid request body.")
		return false
	}
	return true
}

func pair(a, b int64) (int64, int64) {
	if a < b {
		return a, b
	}
	return b, a
}

func isBlockedEitherWay(ctx context.Context, a, b int64) bool {
	var n int
	_ = msgDB.QueryRowContext(ctx,
		`SELECT count(*) FROM social_blocks WHERE (blocker=$1 AND blocked=$2) OR (blocker=$2 AND blocked=$1)`,
		a, b).Scan(&n)
	return n > 0
}

func areFriends(ctx context.Context, a, b int64) bool {
	lo, hi := pair(a, b)
	var st string
	err := msgDB.QueryRowContext(ctx, `SELECT status FROM friendships WHERE user_lo=$1 AND user_hi=$2`, lo, hi).Scan(&st)
	return err == nil && st == "accepted"
}

// memberRole is the authorization check for a conversation: "" means the user
// is not in it and must be refused.
func memberRole(ctx context.Context, conv, user int64) (role string, accepted bool) {
	err := msgDB.QueryRowContext(ctx,
		`SELECT role, accepted FROM conversation_members WHERE conv_id=$1 AND user_id=$2`, conv, user).
		Scan(&role, &accepted)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return "", false
	}
	return role, accepted
}

func convMemberIDs(ctx context.Context, conv int64) []int64 {
	rows, err := msgDB.QueryContext(ctx, `SELECT user_id FROM conversation_members WHERE conv_id=$1`, conv)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func validUsername(s string) bool {
	if len(s) < 3 || len(s) > 24 {
		return false
	}
	for _, c := range s {
		ok := c == '_' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// msgLimiter caps writes per user so one account cannot flood a chat.
var msgLimiter = newIPRateLimiter(time.Minute, 90, 20000)

func msgAllow(w http.ResponseWriter, userID int64) bool {
	if !msgLimiter.Allow("u" + itoa(userID)) {
		jsonError(w, http.StatusTooManyRequests, "rate_limited", "Slow down a little.")
		return false
	}
	return true
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
