package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/KabirSinghBhatia/BitChord/backend/config"
)

// accountMux wires the account endpoints (and the vault, for the sync check)
// onto a fresh in-memory store. The rate limiter is left nil so a test can
// register in a loop without tripping it.
func accountMux() *http.ServeMux {
	accountStore = newMemoryAccountStore()
	authLimiter = nil
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/register", handleAuthRegister)
	mux.HandleFunc("POST /api/auth/login", handleAuthLogin)
	mux.HandleFunc("POST /api/auth/google", handleAuthGoogle)
	mux.HandleFunc("GET /api/auth/me", handleAuthMe)
	mux.HandleFunc("POST /api/auth/logout", handleAuthLogout)
	mux.HandleFunc("GET /api/admin/stats", handleAdminStats)
	mux.HandleFunc("GET /api/vault/{userId}", handleVaultGet)
	mux.HandleFunc("PUT /api/vault/{userId}", handleVaultPut)
	return mux
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path, token string, body any, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	decoded := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	}
	return rec, decoded
}

// register returns the bearer token and the account's vault id.
func register(t *testing.T, mux *http.ServeMux, email, password string) (string, string, map[string]any) {
	t.Helper()
	rec, body := doJSON(t, mux, http.MethodPost, "/api/auth/register", "", map[string]string{
		"email":    email,
		"password": password,
		"name":     "Test Person",
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body = %s", rec.Code, rec.Body.String())
	}
	token, _ := body["token"].(string)
	user, _ := body["user"].(map[string]any)
	vaultID, _ := user["vaultId"].(string)
	if token == "" || vaultID == "" {
		t.Fatalf("register returned no token/vaultId: %s", rec.Body.String())
	}
	if !validVaultId(vaultID) {
		t.Fatalf("vaultId %q is not a 32-hex key the vault accepts", vaultID)
	}
	return token, vaultID, user
}

func TestAccountRegisterLoginMeLogout(t *testing.T) {
	mux := accountMux()

	token, vaultID, user := register(t, mux, "Ann@Example.com", "hunter2hunter")
	if user["email"] != "Ann@Example.com" {
		t.Fatalf("email echoed as %v", user["email"])
	}
	if user["loginCount"].(float64) != 1 {
		t.Fatalf("a new account should already be at one login, got %v", user["loginCount"])
	}
	if user["hasPassword"] != true || user["hasGoogle"] != false {
		t.Fatalf("flags wrong: %v", user)
	}

	// me resolves the token to the same account.
	rec, body := doJSON(t, mux, http.MethodGet, "/api/auth/me", token, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me status = %d, body = %s", rec.Code, rec.Body.String())
	}
	me := body["user"].(map[string]any)
	if me["vaultId"] != vaultID {
		t.Fatalf("me vaultId = %v, want %s", me["vaultId"], vaultID)
	}

	// A second sign-in returns a token too (and bumps the count).
	rec, body = doJSON(t, mux, http.MethodPost, "/api/auth/login", "", map[string]string{
		"email":    "ann@example.com", // case-insensitive, like normalizeEmail
		"password": "hunter2hunter",
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second, _ := body["token"].(string)
	if second == "" || second == token {
		t.Fatalf("login should hand back a fresh token, got %q", second)
	}
	if body["user"].(map[string]any)["loginCount"].(float64) != 2 {
		t.Fatalf("login count after two sign-ins = %v", body["user"].(map[string]any)["loginCount"])
	}

	// Logout kills that token and only that token.
	rec, _ = doJSON(t, mux, http.MethodPost, "/api/auth/logout", second, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d", rec.Code)
	}
	if rec, _ := doJSON(t, mux, http.MethodGet, "/api/auth/me", second, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out token still works: %d", rec.Code)
	}
	if rec, _ := doJSON(t, mux, http.MethodGet, "/api/auth/me", token, nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("the other session should survive, got %d", rec.Code)
	}
}

func TestAccountRejectsDuplicateEmailAndBadCredentials(t *testing.T) {
	mux := accountMux()
	register(t, mux, "dup@example.com", "password123")

	rec, _ := doJSON(t, mux, http.MethodPost, "/api/auth/register", "", map[string]string{
		"email": "DUP@example.com", "password": "password123",
	}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate register status = %d, want 409", rec.Code)
	}

	rec, _ = doJSON(t, mux, http.MethodPost, "/api/auth/login", "", map[string]string{
		"email": "dup@example.com", "password": "wrong-password",
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", rec.Code)
	}

	rec, _ = doJSON(t, mux, http.MethodPost, "/api/auth/login", "", map[string]string{
		"email": "nobody@example.com", "password": "password123",
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown email status = %d, want 401", rec.Code)
	}

	rec, _ = doJSON(t, mux, http.MethodPost, "/api/auth/register", "", map[string]string{
		"email": "not-an-email", "password": "password123",
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad email status = %d, want 422", rec.Code)
	}

	rec, _ = doJSON(t, mux, http.MethodPost, "/api/auth/register", "", map[string]string{
		"email": "short@example.com", "password": "short",
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("short password status = %d, want 422", rec.Code)
	}
}

func TestAccountAdminStats(t *testing.T) {
	mux := accountMux()
	register(t, mux, "one@example.com", "password123")

	// Off by default: no key means the endpoint does not exist.
	t.Setenv("JAM_ADMIN_KEY", "")
	config.AdminKey = ""
	if rec, _ := doJSON(t, mux, http.MethodGet, "/api/admin/stats", "", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unconfigured admin status = %d, want 404", rec.Code)
	}

	config.AdminKey = "super-secret"
	defer func() { config.AdminKey = "" }()

	if rec, _ := doJSON(t, mux, http.MethodGet, "/api/admin/stats", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401", rec.Code)
	}
	if rec, _ := doJSON(t, mux, http.MethodGet, "/api/admin/stats", "", nil, map[string]string{"X-Admin-Key": "nope"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d, want 401", rec.Code)
	}

	rec, body := doJSON(t, mux, http.MethodGet, "/api/admin/stats", "", nil, map[string]string{"X-Admin-Key": "super-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d, body = %s", rec.Code, rec.Body.String())
	}
	stats := body["stats"].(map[string]any)
	if stats["totalUsers"].(float64) != 1 {
		t.Fatalf("totalUsers = %v, want 1", stats["totalUsers"])
	}
	if stats["totalLogins"].(float64) != 1 {
		t.Fatalf("totalLogins = %v, want 1", stats["totalLogins"])
	}
	if stats["activeSessions"].(float64) != 1 {
		t.Fatalf("activeSessions = %v, want 1", stats["activeSessions"])
	}
}

// TestAccountVaultSyncAcrossDevices is the whole point of the feature: sign in
// with one account on two devices and the listening minutes add up.
func TestAccountVaultSyncAcrossDevices(t *testing.T) {
	t.Setenv("VAULT_PATH", filepath.Join(t.TempDir(), "vault.json"))
	vaultLoad()
	mux := accountMux()

	_, vaultID, _ := register(t, mux, "sync@example.com", "password123")
	_, month, day := todayParts()

	put := func(device string, ms int64) {
		raw, err := json.Marshal(vaultPush{
			DeviceID: device,
			Buckets: []vaultBucket{{
				Version: 1,
				Month:   month,
				Days:    map[string]int64{day: ms},
			}},
		})
		if err != nil {
			t.Fatalf("marshal push: %v", err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/vault/"+vaultID, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT for %s = %d, %s", device, rec.Code, rec.Body.String())
		}
	}
	put("phone-device", 500)
	put("pc-device", 500)

	get := httptest.NewRequest(http.MethodGet, "/api/vault/"+vaultID, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET vault = %d, %s", rec.Code, rec.Body.String())
	}
	var buckets []vaultBucket
	if err := json.Unmarshal(rec.Body.Bytes(), &buckets); err != nil {
		t.Fatalf("decode vault: %v", err)
	}
	var total int64
	for _, bucket := range buckets {
		if bucket.Month == month {
			if iso, ok := isoDayKey(month, day); ok {
				total = bucket.DaysISO[iso]
			}
		}
	}
	if total != 1000 {
		t.Fatalf("phone 500 + pc 500 merged to %d, want 1000", total)
	}
}

// TestAccountGoogleSignIn exercises the Google path with a stubbed tokeninfo
// endpoint, so no real Google call is made.
func TestAccountGoogleSignIn(t *testing.T) {
	oldClient := googleTokenClient
	oldClientID := config.GoogleClientID
	oldDesktopID := config.GoogleDesktopClientID
	defer func() {
		googleTokenClient = oldClient
		config.GoogleClientID = oldClientID
		config.GoogleDesktopClientID = oldDesktopID
	}()
	config.GoogleClientID = "test-client-id"
	config.GoogleDesktopClientID = ""

	var lastURL string
	googleTokenClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		lastURL = r.URL.String()
		payload := map[string]any{
			"sub":            "google-sub-1",
			"email":          "Gita@Example.com",
			"email_verified": "true",
			"name":           "Gita",
			"aud":            "test-client-id",
		}
		raw, _ := json.Marshal(payload)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
		}, nil
	})}

	mux := accountMux()
	rec, body := doJSON(t, mux, http.MethodPost, "/api/auth/google", "", map[string]string{"idToken": "stub"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("google sign-in = %d, body = %s", rec.Code, rec.Body.String())
	}
	user := body["user"].(map[string]any)
	if user["hasGoogle"] != true || user["email"] != "Gita@Example.com" {
		t.Fatalf("google account wrong: %v", user)
	}
	firstID := user["id"]
	if lastURL == "" {
		t.Fatal("tokeninfo was never called")
	}

	// Signing in again is a login, not a second account.
	rec, body = doJSON(t, mux, http.MethodPost, "/api/auth/google", "", map[string]string{"idToken": "stub"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second google sign-in = %d", rec.Code)
	}
	if body["user"].(map[string]any)["id"] != firstID {
		t.Fatalf("second google sign-in made a new account: %v vs %v", body["user"].(map[string]any)["id"], firstID)
	}
}

func TestAccountGoogleRejectsWrongAudience(t *testing.T) {
	oldClient := googleTokenClient
	oldClientID := config.GoogleClientID
	oldDesktopID := config.GoogleDesktopClientID
	defer func() {
		googleTokenClient = oldClient
		config.GoogleClientID = oldClientID
		config.GoogleDesktopClientID = oldDesktopID
	}()
	config.GoogleClientID = "test-client-id"
	config.GoogleDesktopClientID = ""
	googleTokenClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := json.Marshal(map[string]any{
			"sub": "s", "email": "x@example.com", "email_verified": true, "aud": "some-other-app",
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
		}, nil
	})}

	mux := accountMux()
	rec, _ := doJSON(t, mux, http.MethodPost, "/api/auth/google", "", map[string]string{"idToken": "stub"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("audience mismatch = %d, want 401", rec.Code)
	}
}

// TestAccountGoogleAcceptsDesktopAudience covers the PC's own OAuth client:
// Android mints tokens for the Web client id, the desktop app for its Desktop
// client id, and both are the same account system.
func TestAccountGoogleAcceptsDesktopAudience(t *testing.T) {
	oldClient := googleTokenClient
	oldClientID := config.GoogleClientID
	oldDesktopID := config.GoogleDesktopClientID
	defer func() {
		googleTokenClient = oldClient
		config.GoogleClientID = oldClientID
		config.GoogleDesktopClientID = oldDesktopID
	}()
	config.GoogleClientID = ""
	config.GoogleDesktopClientID = "desktop-client-id"
	googleTokenClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := json.Marshal(map[string]any{
			"sub": "desktop-sub-1", "email": "desktop@example.com",
			"email_verified": "true", "name": "Desktop", "aud": "desktop-client-id",
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
		}, nil
	})}

	mux := accountMux()
	rec, body := doJSON(t, mux, http.MethodPost, "/api/auth/google", "", map[string]string{"idToken": "stub"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("desktop google sign-in = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body["user"].(map[string]any)["email"] != "desktop@example.com" {
		t.Fatalf("desktop account wrong: %v", body["user"])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
