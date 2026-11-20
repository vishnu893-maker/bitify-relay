package main

// Total users over HTTP (2026-11-20): register an installation, link an
// installation to the signed-in account, and read the running total.
//
// The endpoints:
//   POST /api/users/install  {installId}                       -> {ok, total}
//   POST /api/users/link     {installId?}  Bearer <token>     -> {ok, total}
//   GET  /api/users/count                                      -> {total, serverMs}
//
// The counting rules live in the store, not here: this file only validates what
// the client sent and refuses what the client must not be trusted with.
//
//   - The client never sends a number. It sends an identity (a UUID it generated
//     and keeps), and the server decides whether that identity is new. A client
//     that calls /install a thousand times adds at most one row.
//   - The account identity is never sent by the client at all. /link reads it
//     from the authenticated session (the vault id the server issued), so a
//     forged body cannot count somebody else's account, or a made-up one.
//   - The install id must be a canonical UUID, the same shape the presence
//     heartbeat already uses. A random short string is rejected, which keeps the
//     table to something bounded rather than free-form text.
//   - /install is per-IP rate limited, so one host cannot mint identities in a
//     tight loop. (It cannot stop a determined party from generating UUIDs
//     slowly; nothing short of device attestation can. It raises the cost and
//     keeps honest clients' retries from ever being throttled.)
//
// This feature is additive and isolated: it touches neither Listen Together nor
// the account flow beyond one best-effort counter call at sign-in. If its store
// is not configured, these three routes answer 503 and nothing else changes.

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/KabirSinghBhatia/BitChord/backend/config"
	"github.com/KabirSinghBhatia/BitChord/backend/presence"
)

var (
	userCounter  UserCounterStore
	usersLimiter *ipRateLimiter
)

// usersInit builds the store and the registration rate limiter. Called once
// from main, after accountInit (whose app_users table the Postgres backfill
// reads). Tests wire their own store and leave this alone.
func usersInit() {
	usersLimiter = newIPRateLimiter(time.Minute, config.UsersRatePerMinute, config.RateLimitMaxEntries)
	userCounter = newUserCounterStore()
}

// newUserCounterStore mirrors newAccountStore: the real store when a database
// is configured, the RAM one only when explicitly asked for, nothing otherwise.
func newUserCounterStore() UserCounterStore {
	if config.DatabaseURL != "" {
		store, err := newPostgresUserCounterStore(config.DatabaseURL)
		if err != nil {
			log.Printf("users: postgres unavailable (%v); the user count is disabled", err)
			return nil
		}
		log.Printf("users: postgres ready")
		return store
	}
	if config.AuthMemory {
		log.Printf("users: in-memory counter (JAM_AUTH_MEMORY=1; testing only)")
		return newMemoryUserCounterStore()
	}
	log.Printf("users: no DATABASE_URL and JAM_AUTH_MEMORY=0; the user count is disabled")
	return nil
}

// usersReady refuses the request when no store is configured.
func usersReady(w http.ResponseWriter) bool {
	if userCounter == nil {
		jsonError(w, http.StatusServiceUnavailable, "users_disabled", "The user count is not configured on this server.")
		return false
	}
	return true
}

// usersAllow applies the per-IP registration rate limit. A nil limiter (tests)
// lets everything through.
func usersAllow(w http.ResponseWriter, r *http.Request) bool {
	if usersLimiter == nil {
		return true
	}
	if !usersLimiter.Allow(clientIP(r)) {
		jsonError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Try again in a minute.")
		return false
	}
	return true
}

// validInstallID is the presence UUID rule, reused so the app's install id is
// accepted in exactly one shape.
func validInstallID(id string) bool {
	_, ok := presence.ParseID(id)
	return ok
}

// noteAccountIdentity records an account in the tally. Best-effort by design:
// counting must never be why a sign-in failed, so an error is logged and the
// caller carries on. Safe to call on every sign-in — the insert is idempotent,
// so a returning user never counts twice.
func noteAccountIdentity(ctx context.Context, vaultID string) {
	if userCounter == nil || vaultID == "" {
		return
	}
	if err := userCounter.CountAccount(ctx, vaultID, time.Now()); err != nil {
		log.Printf("users: could not count account %s: %v", vaultID, err)
	}
}

func handleUserInstall(w http.ResponseWriter, r *http.Request) {
	if !usersReady(w) || !usersAllow(w, r) {
		return
	}
	var body struct {
		InstallID string `json:"installId"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if !validInstallID(body.InstallID) {
		jsonError(w, http.StatusUnprocessableEntity, "bad_install_id", "installId must be a UUID.")
		return
	}
	ctx := r.Context()
	if err := userCounter.CountInstall(ctx, body.InstallID, time.Now()); err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not count this installation.")
		return
	}
	writeUserTotal(w, ctx, http.StatusOK)
}

func handleUserLink(w http.ResponseWriter, r *http.Request) {
	if !usersReady(w) || !usersAllow(w, r) {
		return
	}
	ctx := r.Context()
	user, _, ok := currentUser(ctx, r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
		return
	}
	var body struct {
		InstallID string `json:"installId"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	// The install is optional: an account signing in on a device that never
	// registered one still deserves to be counted. When it is present it must
	// be well-formed.
	if body.InstallID != "" {
		if !validInstallID(body.InstallID) {
			jsonError(w, http.StatusUnprocessableEntity, "bad_install_id", "installId must be a UUID.")
			return
		}
		if err := userCounter.CountInstall(ctx, body.InstallID, time.Now()); err != nil {
			jsonError(w, http.StatusInternalServerError, "server_error", "Could not count this installation.")
			return
		}
	}
	// The identity is the account's own vault id, resolved from the session —
	// never anything the client put in the body.
	if err := userCounter.CountAccount(ctx, user.VaultID, time.Now()); err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not count this account.")
		return
	}
	writeUserTotal(w, ctx, http.StatusOK)
}

func handleUserCount(w http.ResponseWriter, r *http.Request) {
	if !usersReady(w) {
		return
	}
	writeUserTotal(w, r.Context(), http.StatusOK)
}

// writeUserTotal answers with the running total. Both the write paths and the
// read path answer the same shape, so the app can update its badge from a
// registration reply without a second request.
func writeUserTotal(w http.ResponseWriter, ctx context.Context, status int) {
	total, err := userCounter.Total(ctx)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not read the user count.")
		return
	}
	jsonResponse(w, status, map[string]interface{}{
		"ok":       true,
		"total":    total,
		"serverMs": time.Now().UnixMilli(),
	})
}
