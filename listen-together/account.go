package main

// App accounts over HTTP (2026-11-07): register, sign in, sign in with Google,
// read the signed-in user, sign out, and one admin view of the numbers.
//
// Everything here is deliberately provider-neutral. A YouTube Music cookie
// still runs the streaming side; this is the app's own login, so a device can
// be handed a vault id from the server and two devices sharing that login share
// their streak and minutes. Passwords are bcrypt; sessions are random bearer
// tokens stored only as a SHA-256 hash, so the database never holds a usable
// token — only what it takes to recognise one.
//
// The endpoints:
//   POST /api/auth/register  {email, password, name?}        -> {token, user}
//   POST /api/auth/login     {email, password}               -> {token, user}
//   POST /api/auth/google    {idToken}                       -> {token, user}
//   GET  /api/auth/me        Authorization: Bearer <token>   -> {user}
//   POST /api/auth/logout    Authorization: Bearer <token>   -> {ok}
//   GET  /api/admin/stats    X-Admin-Key: <JAM_ADMIN_KEY>    -> {stats}

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/KabirSinghBhatia/BitChord/backend/config"
)

const (
	authMinPassword    = 8
	authMaxPassword    = 128
	authMaxName        = 60
	authMaxEmail       = 254
	authVaultIDRetries = 5
)

var (
	accountStore AccountStore
	authLimiter  *ipRateLimiter
)

// accountInit builds the store and the login rate limiter. Called once from
// main; tests wire their own store and leave this alone.
func accountInit() {
	authLimiter = newIPRateLimiter(time.Minute, config.AuthRatePerMinute, config.RateLimitMaxEntries)
	accountStore = newAccountStore()
}

// newAccountStore picks the real store when a database is configured, the RAM
// one only when explicitly asked for, and nothing otherwise.
func newAccountStore() AccountStore {
	if config.DatabaseURL != "" {
		store, err := newPostgresAccountStore(config.DatabaseURL)
		if err != nil {
			// Not fatal: the listen-together side keeps working and the account
			// endpoints answer 503 until the database is reachable.
			log.Printf("accounts: postgres unavailable (%v); account endpoints disabled", err)
			return nil
		}
		log.Printf("accounts: postgres ready")
		return store
	}
	if config.AuthMemory {
		log.Printf("accounts: in-memory store (JAM_AUTH_MEMORY=1; testing only)")
		return newMemoryAccountStore()
	}
	log.Printf("accounts: no DATABASE_URL and JAM_AUTH_MEMORY=0; account endpoints disabled")
	return nil
}

// accountsReady refuses the request when no store is configured.
func accountsReady(w http.ResponseWriter) bool {
	if accountStore == nil {
		jsonError(w, http.StatusServiceUnavailable, "accounts_disabled", "Accounts are not configured on this server.")
		return false
	}
	return true
}

// authAllow applies the per-IP login rate limit. A nil limiter (tests) lets
// everything through.
func authAllow(w http.ResponseWriter, r *http.Request) bool {
	if authLimiter == nil {
		return true
	}
	if !authLimiter.Allow(clientIP(r)) {
		jsonError(w, http.StatusTooManyRequests, "rate_limited", "Too many attempts. Try again in a minute.")
		return false
	}
	return true
}

// randomHex returns n random bytes as 2n lowercase hex characters.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// hashToken is what the server stores for a bearer token: the hash, never the
// token, so a database dump cannot sign anyone in.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// issueSession mints a token, stores its hash, and returns the token to hand
// to the client. The raw token leaves the process exactly once.
func issueSession(ctx context.Context, userID int64, now time.Time) (string, time.Time, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := now.Add(time.Duration(config.AuthSessionDays) * 24 * time.Hour)
	if err := accountStore.CreateSession(ctx, hashToken(token), userID, now, expires); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// accountUserView is the account as the client sees it. The bcrypt hash and the
// Google subject never appear; the vault id does, because it is the client's key
// to its own stats.
func accountUserView(user AccountUser) map[string]interface{} {
	return map[string]interface{}{
		"id":          user.ID,
		"email":       user.Email,
		"name":        user.Name,
		"vaultId":     user.VaultID,
		"createdAt":   user.CreatedAt.UnixMilli(),
		"lastLoginAt": user.LastLoginAt.UnixMilli(),
		"loginCount":  user.LoginCount,
		"hasPassword": user.PasswordHash != "",
		"hasGoogle":   user.GoogleSub != "",
	}
}

// currentUser resolves the bearer token to a live session and its account.
func currentUser(ctx context.Context, r *http.Request) (AccountUser, AccountSession, bool) {
	token := parseBearerToken(r)
	if token == "" {
		return AccountUser{}, AccountSession{}, false
	}
	now := time.Now()
	session, ok, err := accountStore.Session(ctx, hashToken(token))
	if err != nil || !ok || !session.ExpiresAt.After(now) {
		return AccountUser{}, AccountSession{}, false
	}
	user, ok, err := accountStore.UserByID(ctx, session.UserID)
	if err != nil || !ok {
		return AccountUser{}, AccountSession{}, false
	}
	return user, session, true
}

func handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !accountsReady(w) || !authAllow(w, r) {
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	email := strings.TrimSpace(body.Email)
	emailNorm := normalizeEmail(email)
	if !validEmail(email) {
		jsonError(w, http.StatusUnprocessableEntity, "bad_email", "Enter a valid email address.")
		return
	}
	if !validPassword(body.Password) {
		jsonError(w, http.StatusUnprocessableEntity, "bad_password", "Password must be at least 8 characters.")
		return
	}
	name := trimName(body.Name, emailNorm)

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not create the account.")
		return
	}

	now := time.Now()
	ctx := r.Context()
	var user AccountUser
	created := false
	for attempt := 0; attempt < authVaultIDRetries && !created; attempt++ {
		vaultID, err := randomHex(16)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "server_error", "Could not create the account.")
			return
		}
		user, err = accountStore.CreateUser(ctx, CreateUserInput{
			Email:        email,
			EmailNorm:    emailNorm,
			Name:         name,
			PasswordHash: string(hash),
			VaultID:      vaultID,
			Now:          now,
		})
		switch {
		case err == nil:
			created = true
		case errors.Is(err, errVaultTaken):
			continue // improbable collision; draw another id
		case errors.Is(err, errEmailTaken):
			jsonError(w, http.StatusConflict, "email_taken", "That email is already registered.")
			return
		default:
			jsonError(w, http.StatusInternalServerError, "server_error", "Could not create the account.")
			return
		}
	}
	if !created {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not create the account.")
		return
	}

	// Creating an account is its first login, so the count starts at one and
	// "active today" already includes it.
	_ = accountStore.RecordLogin(ctx, user.ID, now)
	user.LoginCount++
	user.LastLoginAt = now

	token, expires, err := issueSession(ctx, user.ID, now)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not start the session.")
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]interface{}{
		"token":     token,
		"expiresAt": expires.UnixMilli(),
		"user":      accountUserView(user),
	})
}

func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !accountsReady(w) || !authAllow(w, r) {
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	emailNorm := normalizeEmail(body.Email)
	if !validEmail(body.Email) || body.Password == "" {
		jsonError(w, http.StatusUnauthorized, "invalid_credentials", "Wrong email or password.")
		return
	}
	ctx := r.Context()
	user, ok, err := accountStore.UserByEmail(ctx, emailNorm)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not sign in.")
		return
	}
	if !ok || user.PasswordHash == "" {
		// Same answer either way: a missing account must not be told apart from
		// a wrong password by anything but the wording below.
		if ok && user.PasswordHash == "" {
			jsonError(w, http.StatusConflict, "use_google", "This account signs in with Google.")
			return
		}
		jsonError(w, http.StatusUnauthorized, "invalid_credentials", "Wrong email or password.")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)) != nil {
		jsonError(w, http.StatusUnauthorized, "invalid_credentials", "Wrong email or password.")
		return
	}
	completeLogin(w, ctx, user)
}

// completeLogin is the shared tail of a successful sign-in: bump the login
// count and hand back a fresh token.
func completeLogin(w http.ResponseWriter, ctx context.Context, user AccountUser) {
	now := time.Now()
	if err := accountStore.RecordLogin(ctx, user.ID, now); err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not sign in.")
		return
	}
	user.LoginCount++
	user.LastLoginAt = now
	token, expires, err := issueSession(ctx, user.ID, now)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not start the session.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"token":     token,
		"expiresAt": expires.UnixMilli(),
		"user":      accountUserView(user),
	})
}

func handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if !accountsReady(w) {
		return
	}
	ctx := r.Context()
	user, session, ok := currentUser(ctx, r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
		return
	}
	// Cheap freshness: note the device was seen without forcing a write on
	// every read path.
	_ = accountStore.TouchSession(ctx, session.TokenHash, time.Now())
	jsonResponse(w, http.StatusOK, map[string]interface{}{"user": accountUserView(user)})
}

func handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !accountsReady(w) {
		return
	}
	if token := parseBearerToken(r); token != "" {
		_ = accountStore.DeleteSession(r.Context(), hashToken(token))
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func handleAuthGoogle(w http.ResponseWriter, r *http.Request) {
	if !accountsReady(w) || !authAllow(w, r) {
		return
	}
	if !config.GoogleEnabled() {
		jsonError(w, http.StatusServiceUnavailable, "google_disabled", "Google Sign-In is not configured on this server.")
		return
	}
	var body struct {
		IDToken string `json:"idToken"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	id, err := verifyGoogleIDToken(r.Context(), body.IDToken)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, "bad_google_token", "Could not verify that Google account.")
		return
	}
	if !bool(id.EmailVerified) || id.Sub == "" || id.Email == "" {
		jsonError(w, http.StatusUnauthorized, "bad_google_token", "That Google account has no verified email.")
		return
	}
	ctx := r.Context()
	emailNorm := normalizeEmail(id.Email)

	// An existing Google link signs straight in.
	if user, ok, err := accountStore.UserByGoogleSub(ctx, id.Sub); err == nil && ok {
		completeLogin(w, ctx, user)
		return
	}
	// Otherwise the same email may already hold a password account: link the
	// two, so one person keeps one vault either way they sign in.
	if user, ok, err := accountStore.UserByEmail(ctx, emailNorm); err == nil && ok {
		if err := accountStore.LinkGoogle(ctx, user.ID, id.Sub); err != nil {
			jsonError(w, http.StatusConflict, "email_taken", "That email is already registered.")
			return
		}
		user.GoogleSub = id.Sub
		completeLogin(w, ctx, user)
		return
	}

	now := time.Now()
	name := trimName(id.Name, emailNorm)
	var user AccountUser
	created := false
	for attempt := 0; attempt < authVaultIDRetries && !created; attempt++ {
		vaultID, err := randomHex(16)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "server_error", "Could not create the account.")
			return
		}
		user, err = accountStore.CreateUser(ctx, CreateUserInput{
			Email:     id.Email,
			EmailNorm: emailNorm,
			Name:      name,
			GoogleSub: id.Sub,
			VaultID:   vaultID,
			Now:       now,
		})
		switch {
		case err == nil:
			created = true
		case errors.Is(err, errVaultTaken):
			continue
		case errors.Is(err, errEmailTaken), errors.Is(err, errGoogleTaken):
			// A racing request created the same person a moment ago; retry the
			// reads rather than declaring a conflict.
			if existing, ok, _ := accountStore.UserByGoogleSub(ctx, id.Sub); ok {
				completeLogin(w, ctx, existing)
				return
			}
			if existing, ok, _ := accountStore.UserByEmail(ctx, emailNorm); ok {
				completeLogin(w, ctx, existing)
				return
			}
			jsonError(w, http.StatusConflict, "email_taken", "That email is already registered.")
			return
		default:
			jsonError(w, http.StatusInternalServerError, "server_error", "Could not create the account.")
			return
		}
	}
	if !created {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not create the account.")
		return
	}
	completeLogin(w, ctx, user)
}

func handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if config.AdminKey == "" {
		// Unset means the endpoint does not exist, rather than existing with a
		// guessable default.
		http.NotFound(w, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Key")), []byte(config.AdminKey)) != 1 {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "A valid admin key is required.")
		return
	}
	if !accountsReady(w) {
		return
	}
	stats, err := accountStore.Stats(r.Context(), time.Now())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not read the stats.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"stats": stats, "serverMs": time.Now().UnixMilli()})
}

// --- validation -------------------------------------------------------------

func validEmail(email string) bool {
	if email == "" || len(email) > authMaxEmail {
		return false
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at != strings.LastIndexByte(email, '@') || at == len(email)-1 {
		return false
	}
	domain := email[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	return !strings.ContainsAny(email, " \t\r\n")
}

func validPassword(password string) bool {
	return len(password) >= authMinPassword && len(password) <= authMaxPassword
}

// trimName falls back to the email's local part so an account always has a name
// to show on the account screen.
func trimName(name, emailNorm string) string {
	name = strings.TrimSpace(name)
	if len(name) > authMaxName {
		name = strings.TrimSpace(name[:authMaxName])
	}
	if name != "" {
		return name
	}
	if at := strings.IndexByte(emailNorm, '@'); at > 0 {
		return emailNorm[:at]
	}
	return "Listener"
}

// --- Google ID token verification -------------------------------------------

// googleIdentity is what we need out of a verified Google ID token.
type googleIdentity struct {
	Sub           string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
	Audience      string   `json:"aud"`
}

// flexBool reads the string-or-boolean the tokeninfo endpoint uses for
// email_verified ("true" and true both appear).
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	value := strings.Trim(string(data), `"`)
	if value == "" || value == "null" {
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return nil
	}
	*b = flexBool(parsed)
	return nil
}

var googleTokenClient = &http.Client{Timeout: 8 * time.Second}

// verifyGoogleIDToken asks Google's tokeninfo endpoint about the ID token and
// checks it was minted for this app. Its answer arrives over TLS from Google
// itself, so trusting it here is the same trust as any Google API call.
func verifyGoogleIDToken(ctx context.Context, idToken string) (googleIdentity, error) {
	if strings.TrimSpace(idToken) == "" || len(idToken) > 8192 {
		return googleIdentity{}, errors.New("empty token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://oauth2.googleapis.com/tokeninfo?id_token="+url.QueryEscape(idToken), nil)
	if err != nil {
		return googleIdentity{}, err
	}
	resp, err := googleTokenClient.Do(req)
	if err != nil {
		return googleIdentity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return googleIdentity{}, errors.New("token rejected")
	}
	var id googleIdentity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&id); err != nil {
		return googleIdentity{}, err
	}
	if !config.GoogleAudienceAllowed(id.Audience) {
		return googleIdentity{}, errors.New("audience mismatch")
	}
	return id, nil
}

// Compile-time proof the memory store satisfies the interface too.
var _ AccountStore = (*memoryAccountStore)(nil)
