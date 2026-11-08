package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/KabirSinghBhatia/BitChord/backend/config"
)

// syncMux is accountMux plus the sync routes, so a test drives the real
// handlers rather than the store directly.
func syncMux() *http.ServeMux {
	mux := accountMux()
	mux.HandleFunc("GET /api/account/sync", handleSyncGet)
	mux.HandleFunc("PUT /api/account/sync", handleSyncPut)
	return mux
}

func TestSyncRoundTripAndConflict(t *testing.T) {
	mux := syncMux()
	token, _, _ := register(t, mux, "sync@example.com", "hunter2hunter2")

	// A brand-new account has no document; the client reads rev 0 and pushes.
	rec, body := doJSON(t, mux, "GET", "/api/account/sync", token, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty get: %d %v", rec.Code, body)
	}
	if body["rev"] != float64(0) || body["doc"] != nil {
		t.Fatalf("empty get body = %v", body)
	}

	doc := map[string]any{
		"playlists": []any{
			map[string]any{"id": "p1", "title": "Late night", "updatedAt": 1},
		},
		"integrations": map[string]any{"discordToken": "secret-token"},
	}
	rec, body = doJSON(t, mux, "PUT", "/api/account/sync", token,
		map[string]any{"baseRev": 0, "doc": doc}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first put: %d %v", rec.Code, body)
	}
	if body["rev"] != float64(1) {
		t.Fatalf("first put rev = %v", body["rev"])
	}

	// The same document comes back, playlists intact.
	rec, body = doJSON(t, mux, "GET", "/api/account/sync", token, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %v", rec.Code, body)
	}
	stored, ok := body["doc"].(map[string]any)
	if !ok {
		t.Fatalf("doc missing: %v", body)
	}
	playlists, _ := stored["playlists"].([]any)
	if len(playlists) != 1 {
		t.Fatalf("playlists = %v", stored["playlists"])
	}

	// A write built on the revision we already replaced is refused, and the
	// answer carries what is actually stored so the client can merge.
	rec, body = doJSON(t, mux, "PUT", "/api/account/sync", token,
		map[string]any{"baseRev": 0, "doc": map[string]any{"playlists": []any{}}}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale put = %d %v", rec.Code, body)
	}
	if body["rev"] != float64(1) {
		t.Fatalf("conflict rev = %v", body["rev"])
	}

	// The next write on the right revision goes through.
	rec, body = doJSON(t, mux, "PUT", "/api/account/sync", token,
		map[string]any{"baseRev": 1, "doc": map[string]any{"playlists": []any{}}}, nil)
	if rec.Code != http.StatusOK || body["rev"] != float64(2) {
		t.Fatalf("second put = %d %v", rec.Code, body)
	}
}

func TestSyncSealsCredentialsAtRest(t *testing.T) {
	previous := config.SyncSecret
	config.SyncSecret = "test-key"
	t.Cleanup(func() { config.SyncSecret = previous })

	mux := syncMux()
	token, _, user := register(t, mux, "sealed@example.com", "hunter2hunter2")
	userID, _ := user["id"].(float64)

	doc := map[string]any{
		"playlists":    []any{},
		"integrations": map[string]any{"discordToken": "very-secret"},
	}
	rec, body := doJSON(t, mux, "PUT", "/api/account/sync", token,
		map[string]any{"baseRev": 0, "doc": doc}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %v", rec.Code, body)
	}

	// What sits in the store is ciphertext...
	stored, ok, err := accountStore.SyncGet(t.Context(), int64(userID))
	if err != nil || !ok {
		t.Fatalf("store read: %v %v", err, ok)
	}
	if !syncSealed(stored.Doc) {
		t.Fatalf("stored document is not sealed: %q", stored.Doc)
	}
	if strings.Contains(stored.Doc, "very-secret") {
		t.Fatal("the Discord token is readable in the stored document")
	}

	// ...and the client still gets the plaintext back.
	rec, body = doJSON(t, mux, "GET", "/api/account/sync", token, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %v", rec.Code, body)
	}
	raw, err := json.Marshal(body["doc"])
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	if !strings.Contains(string(raw), "very-secret") {
		t.Fatalf("token did not come back: %s", raw)
	}
}

func TestSyncRequiresASession(t *testing.T) {
	mux := syncMux()
	if rec, _ := doJSON(t, mux, "GET", "/api/account/sync", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous get = %d", rec.Code)
	}
	if rec, _ := doJSON(t, mux, "PUT", "/api/account/sync", "", map[string]any{"doc": map[string]any{}}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous put = %d", rec.Code)
	}
}
