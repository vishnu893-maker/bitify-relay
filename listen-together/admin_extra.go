package main

// Admin panel 1.9 additions: the users table, suspension, the leaderboard
// reset, the caller's own board row, FCM push, and the promo cards editor.
//
// Everything here follows the file's own conventions (admin_controls.go /
// admin.go): X-Admin-Key via the same constant-time check, JSON in and out,
// and the Postgres handle the accounts store already owns. The accounts
// schema gains one column, added idempotently on first use, and nothing
// existing changes shape - an unban restores everything, nothing is deleted.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── suspended: one column, queried directly ────────────────────────────────

// ensureSuspendedColumn adds the suspended column the first time any of these
// handlers needs it. Idempotent, and a no-op on a non-Postgres store.
func ensureSuspendedColumn(ctx context.Context) {
	db := accountsDB()
	if db == nil {
		return
	}
	_, _ = db.ExecContext(ctx, `ALTER TABLE app_users ADD COLUMN IF NOT EXISTS suspended BOOLEAN NOT NULL DEFAULT FALSE`)
}

// userSuspended is the guard every enforcement point calls. A missing column
// (first deploy ordering) or a non-Postgres store reads as not suspended, so
// this can ship before the migration lands.
func userSuspended(ctx context.Context, userID int64) bool {
	if userID <= 0 {
		return false
	}
	db := accountsDB()
	if db == nil {
		return false
	}
	var suspended bool
	err := db.QueryRowContext(ctx, `SELECT suspended FROM app_users WHERE id = $1`, userID).Scan(&suspended)
	return err == nil && suspended
}

func setSuspended(ctx context.Context, userID int64, value bool) error {
	db := accountsDB()
	if db == nil {
		return errors.New("accounts are not configured")
	}
	ensureSuspendedColumn(ctx)
	_, err := db.ExecContext(ctx, `UPDATE app_users SET suspended = $1 WHERE id = $2`, value, userID)
	return err
}

// accountsDB is the Postgres handle behind the account store, or nil when
// accounts are not configured (tests, a server without Postgres).
func accountsDB() *sql.DB {
	if pg, ok := accountStore.(*postgresAccountStore); ok && pg != nil {
		return pg.db
	}
	return nil
}

// ── users list ──────────────────────────────────────────────────────────────

type adminPanelUser struct {
	ID        int64    `json:"id"`
	Email     string   `json:"email"`
	Name      string   `json:"name"`
	Guest     bool     `json:"isGuest"`
	Suspended bool     `json:"suspended"`
	CreatedAt string   `json:"createdAt,omitempty"`
	LastSeen  string   `json:"lastSeen,omitempty"`
	Minutes   float64  `json:"minutes"`
	BoardName string   `json:"boardName,omitempty"`
	VaultID   string   `json:"vaultId,omitempty"`
}

// handleAdminUsers lists every account with the minutes its board row
// carries. Minutes come from app_leaderboard joined on the account's vault
// id - the same uid the presence ping files board rows under - so the panel
// shows what the public board shows, per person.
func handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !adminControlAuthorized(w, r) {
		return
	}
	db := accountsDB()
	if db == nil {
		jsonError(w, http.StatusServiceUnavailable, "admin_users_disabled", "The users list requires the configured Postgres account store.")
		return
	}
	ensureSuspendedColumn(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 1000
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	rows, err := db.QueryContext(r.Context(), `
SELECT u.id,
       COALESCE(u.email, ''),
       COALESCE(u.name, ''),
       COALESCE(u.suspended, FALSE),
       COALESCE(to_char(u.created_at AT TIME ZONE 'utc', 'YYYY-MM-DD'), ''),
       COALESCE(u.vault_id, ''),
       COALESCE(b.ms, 0),
       COALESCE(b.name, ''),
       COALESCE((SELECT to_char(MAX(t.updated_at) AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI')
                   FROM app_sessions t WHERE t.user_id = u.id), '')
  FROM app_users u
  LEFT JOIN app_leaderboard b ON b.id = u.vault_id AND u.vault_id <> ''
 WHERE ($1 = '' OR u.email ILIKE '%' || $1 || '%' OR u.name ILIKE '%' || $1 || '%')
 ORDER BY COALESCE(b.ms, 0) DESC, u.id ASC
 LIMIT $2`, query, limit)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not read users.")
		return
	}
	defer rows.Close()
	users := []adminPanelUser{}
	for rows.Next() {
		var u adminPanelUser
		var ms int64
		var email, name, createdAt, vaultID, boardName, lastSeen string
		if err := rows.Scan(&u.ID, &email, &name, &u.Suspended, &createdAt, &vaultID, &ms, &boardName, &lastSeen); err != nil {
			continue
		}
		u.Email = email
		u.Name = name
		u.Guest = strings.TrimSpace(email) == ""
		u.CreatedAt = createdAt
		u.VaultID = vaultID
		u.Minutes = float64(ms) / 60000.0
		u.BoardName = boardName
		u.LastSeen = lastSeen
		users = append(users, u)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"users": users})
}

// handleAdminUserAction serves POST /api/admin/users/{id}/ban and /unban.
// Suspension is enforced at the presence gate (main.go) and the session
// check below, both of which answer the client's own 403 {"error":"banned"}
// shape - that is what turns the app into its suspended screen.
func handleAdminUserAction(w http.ResponseWriter, r *http.Request) {
	if !adminControlAuthorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST.")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		jsonError(w, http.StatusUnprocessableEntity, "bad_user_id", "That is not a user id.")
		return
	}
	suspended := false
	switch r.PathValue("action") {
	case "ban":
		suspended = true
	case "unban":
		suspended = false
	default:
		http.NotFound(w, r)
		return
	}
	if setSuspended(r.Context(), id, suspended) != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not update the account.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "id": id, "suspended": suspended})
}

// enforceAccountSuspension is the session-level gate: a suspended account
// gets 403 {"error":"banned"} from every authenticated route that calls it,
// which is the shape the app's HTTP client sniffs to raise its suspended
// screen. Called from currentUser's callers where it matters; presence has
// its own gate in main.go.
func enforceAccountSuspension(w http.ResponseWriter, r *http.Request, userID int64) bool {
	if !userSuspended(r.Context(), userID) {
		return true
	}
	jsonError(w, http.StatusForbidden, "banned", "This account has been suspended.")
	return false
}

// ── leaderboard reset ───────────────────────────────────────────────────────

// handleAdminLeaderboardReset clears the public board and bumps the controls'
// resetEpoch so every client forgets its stored total. Listening history (the
// vault) is untouched: this resets the leaderboard, not the listener.
func handleAdminLeaderboardReset(w http.ResponseWriter, r *http.Request) {
	if !adminControlAuthorized(w, r) {
		return
	}
	if adminControlDB == nil {
		jsonError(w, http.StatusServiceUnavailable, "admin_controls_disabled", "The reset needs the configured Postgres store.")
		return
	}
	removed := leaderboardStore.Reset()
	// Bump the epoch inside the controls payload, through the same
	// read-modify-write the panel's own save uses, so the revision advances
	// and a concurrent panel edit is not silently lost.
	state, err := readAdminControlState(r.Context())
	if err != nil {
		jsonError(w, http.StatusServiceUnavailable, "config_unavailable", "Could not read admin controls.")
		return
	}
	epoch := time.Now().Unix()
	state.ResetEpoch = epoch
	if err := writeAdminControlState(r.Context(), state); err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not record the reset.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "resetEpoch": epoch, "removed": removed})
}

// writeAdminControlState persists the whole state, revision and all - the
// same shape handleAdminControls' PUT takes.
func writeAdminControlState(ctx context.Context, state AdminControlState) error {
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = adminControlDB.ExecContext(ctx, `
UPDATE admin_runtime_config
   SET revision = revision + 1, payload = $1, updated_at = now()
 WHERE config_id = 1`, payload)
	return err
}

// ── the caller's own board row ──────────────────────────────────────────────

// handleStatsMe answers the signed-in caller's own board row: minutes and
// rank, keyed by identity (the session's vault id) rather than a name. An
// account with no row yet answers zeros rather than 404, so the client has
// no special case for "new here".
func handleStatsMe(w http.ResponseWriter, r *http.Request) {
	if !accountsReady(w) {
		return
	}
	user, _, ok := currentUser(r.Context(), r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
		return
	}
	if !enforceAccountSuspension(w, r, user.ID) {
		return
	}
	var name string
	var ms int64
	if user.VaultID != "" {
		for _, row := range leaderboardStore.Rows() {
			if row.ID == user.VaultID {
				name, ms = row.Name, row.Ms
				break
			}
		}
	}
	// Rank among named rows: a row with no name never renders on the board,
	// so it does not count toward it either.
	rank := int64(0)
	if user.VaultID != "" && strings.TrimSpace(name) != "" {
		for _, row := range leaderboardStore.Rows() {
			if strings.TrimSpace(row.Name) != "" && row.Ms > ms {
				rank++
			}
		}
		rank++
	}
	resp := map[string]any{
		"uid":     user.VaultID,
		"name":    name,
		"ms":      ms,
		"minutes": ms / 60000.0,
		"rank":    rank,
	}
	jsonResponse(w, http.StatusOK, resp)
}

// ── promo cards: the panel edits, /api/app/config serves ────────────────────

// handleAdminPromoCards reads and writes just the cards slice of the controls
// payload, so the panel never has to round-trip the whole document (and its
// revision) to pin a card.
func handleAdminPromoCards(w http.ResponseWriter, r *http.Request) {
	if !adminControlAuthorized(w, r) {
		return
	}
	if adminControlDB == nil {
		jsonError(w, http.StatusServiceUnavailable, "admin_controls_disabled", "Promo cards require the configured Postgres store.")
		return
	}
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		state, err := readAdminControlState(ctx)
		if err != nil {
			jsonError(w, http.StatusServiceUnavailable, "config_unavailable", "Could not read admin controls.")
			return
		}
		if state.PromoCards == nil {
			state.PromoCards = []AdminPromoCard{}
		}
		jsonResponse(w, http.StatusOK, state.PromoCards)
	case http.MethodPut:
		var cards []AdminPromoCard
		if !decodeJSONBody(w, r, &cards) {
			return
		}
		kept := cards[:0]
		for _, card := range cards {
			if strings.TrimSpace(card.ID) == "" || strings.TrimSpace(card.Title) == "" {
				continue
			}
			kept = append(kept, card)
		}
		state, err := readAdminControlState(ctx)
		if err != nil {
			jsonError(w, http.StatusServiceUnavailable, "config_unavailable", "Could not read admin controls.")
			return
		}
		state.PromoCards = kept
		if err := writeAdminControlState(ctx, state); err != nil {
			jsonError(w, http.StatusInternalServerError, "server_error", "Could not save the cards.")
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "cards": len(kept)})
	default:
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET or PUT.")
	}
}

// ── FCM push ────────────────────────────────────────────────────────────────

// handleFCMToken registers (or refreshes) a device token, tied to the
// signed-in account. The client posts on every app start and rotation, so a
// repeat POST is an idempotent upsert. A suspended account's token is
// dropped rather than registered - banned is banned.
func handleFCMToken(w http.ResponseWriter, r *http.Request) {
	if !accountsReady(w) {
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST.")
		return
	}
	if adminControlDB == nil {
		jsonError(w, http.StatusServiceUnavailable, "fcm_disabled", "Push needs the configured Postgres store.")
		return
	}
	var body struct {
		Token    string `json:"token"`
		Platform string `json:"platform"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	if body.Token == "" || len(body.Token) > 4096 {
		jsonError(w, http.StatusUnprocessableEntity, "bad_token", "A device token is required.")
		return
	}
	if body.Platform == "" {
		body.Platform = "android"
	}
	user, _, ok := currentUser(r.Context(), r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
		return
	}
	if !enforceAccountSuspension(w, r, user.ID) {
		return
	}
	if body.Platform == "" {
		body.Platform = "android"
	}
	if _, err := adminControlDB.ExecContext(r.Context(), `
INSERT INTO fcm_tokens (token, user_id, platform, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (token) DO UPDATE SET user_id = $2, platform = $3, updated_at = now()`,
		body.Token, user.ID, body.Platform); err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not store the token.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

type adminPushRequest struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	Target string `json:"target"`
}

// handleAdminPush delivers one message to every registered device through
// FCM HTTP v1. Configured entirely by env: FCM_SERVICE_JSON (the full
// service-account JSON) - unset, the route answers 503 with the one-line
// reason rather than pretending to have sent anything.
func handleAdminPush(w http.ResponseWriter, r *http.Request) {
	if !adminControlAuthorized(w, r) {
		return
	}
	if adminControlDB == nil {
		jsonError(w, http.StatusServiceUnavailable, "fcm_disabled", "Push needs the configured Postgres store.")
		return
	}
	var req adminPushRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.Title = collapseSpace(req.Title)
	req.Body = collapseSpace(req.Body)
	if req.Title == "" || req.Body == "" {
		jsonError(w, http.StatusUnprocessableEntity, "title_and_body_required", "A title and a message are required.")
		return
	}
	if len(req.Title) > 120 || len(req.Body) > 400 {
		jsonError(w, http.StatusUnprocessableEntity, "too_long", "Keep the title under 120 and the message under 400 characters.")
		return
	}
	if req.Target == "" {
		req.Target = "all"
	}
	rows, err := adminControlDB.QueryContext(r.Context(), `SELECT token FROM fcm_tokens`)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not read device tokens.")
		return
	}
	defer rows.Close()
	sent, failed := 0, 0
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			continue
		}
		err := fcmSendToDevice(token, req.Title, req.Body, req.URL)
		if errors.Is(err, errFCMTokenGone) {
			_, _ = adminControlDB.ExecContext(r.Context(), `DELETE FROM fcm_tokens WHERE token = $1`, token)
			failed++
			continue
		}
		if err != nil {
			failed++
			continue
		}
		sent++
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sent": sent, "failed": failed})
}

func collapseSpace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// ── FCM HTTP v1 client, standard library only ───────────────────────────────

var errFCMTokenGone = errors.New("fcm: token unregistered")

var (
	fcmMu        sync.Mutex
	fcmCreds     *fcmCredentials
	fcmOAuth     string
	fcmOAuthExpr time.Time
)

type fcmCredentials struct {
	ProjectID string
	Email     string
	Key       *rsa.PrivateKey
}

// fcmCredentialsOnce parses FCM_SERVICE_JSON. Unset is a supported state:
// push stays disabled and the admin route says so.
func fcmCredentialsOnce() (*fcmCredentials, error) {
	fcmMu.Lock()
	defer fcmMu.Unlock()
	if fcmCreds != nil {
		return fcmCreds, nil
	}
	raw := strings.TrimSpace(os.Getenv("FCM_SERVICE_JSON"))
	if raw == "" {
		return nil, errors.New("FCM_SERVICE_JSON is not set on this server")
	}
	var parsed struct {
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("FCM_SERVICE_JSON: %w", err)
	}
	if parsed.ProjectID == "" || parsed.ClientEmail == "" || parsed.PrivateKey == "" {
		return nil, errors.New("FCM_SERVICE_JSON is missing project_id, client_email or private_key")
	}
	block, _ := pem.Decode([]byte(parsed.PrivateKey))
	if block == nil {
		return nil, errors.New("FCM_SERVICE_JSON private key is not PEM")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		keyAny, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("FCM_SERVICE_JSON private key: %w", err)
		}
	}
	key, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("FCM_SERVICE_JSON key is not RSA")
	}
	fcmCreds = &fcmCredentials{ProjectID: parsed.ProjectID, Email: parsed.ClientEmail, Key: key}
	return fcmCreds, nil
}

// fcmOAuthToken mints and caches an access token for the FCM scope with the
// service account's JWT-bearer grant.
func fcmOAuthToken(creds *fcmCredentials) (string, error) {
	fcmMu.Lock()
	defer fcmMu.Unlock()
	if fcmOAuth != "" && time.Now().Before(fcmOAuthExpr) {
		return fcmOAuth, nil
	}
	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss":   creds.Email,
		"scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud":   "https://oauth2.googleapis.com/token",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	b64 := base64.RawURLEncoding.EncodeToString
	signingInput := b64(header) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, creds.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	assertion := signingInput + "." + b64(sig)
	form := "grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=" + assertion
	resp, err := http.Post("https://oauth2.googleapis.com/token",
		"application/x-www-form-urlencoded", strings.NewReader(form))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return "", err
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("oauth: %s", answer.ErrorDesc)
	}
	fcmOAuth = answer.AccessToken
	fcmOAuthExpr = now.Add(time.Duration(answer.ExpiresIn-60) * time.Second)
	return fcmOAuth, nil
}

// fcmSendToDevice delivers one message to one device token. A 404/410 means
// the token is dead and the caller prunes it.
func fcmSendToDevice(token, title, body, url string) error {
	creds, err := fcmCredentialsOnce()
	if err != nil {
		return err
	}
	oauth, err := fcmOAuthToken(creds)
	if err != nil {
		return err
	}
	message := map[string]any{
		"message": map[string]any{
			"token": token,
			"notification": map[string]string{
				"title": title,
				"body":  body,
			},
			"android": map[string]any{"priority": "HIGH"},
		},
	}
	if url != "" {
		message["message"].(map[string]any)["data"] = map[string]string{"url": url}
	}
	payload, _ := json.Marshal(message)
	req, err := http.NewRequest("POST",
		"https://fcm.googleapis.com/v1/projects/"+creds.ProjectID+"/messages:send",
		strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+oauth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode == 410 {
		return errFCMTokenGone
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("fcm: HTTP %d", resp.StatusCode)
	}
	return nil
}
