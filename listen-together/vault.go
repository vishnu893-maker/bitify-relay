package main

// The listening vault (2026-10-24): play minutes that survive a reinstall.
//
// A signed-in device (web, phone, desktop) keeps its Replay stats locally, so
// downloading the app again wipes every minute of it. This vault ties the
// stats to the account instead: the client uploads its month buckets keyed by
// a hash of its login — never the token — and pulls them back on a fresh
// install, merging in what it doesn't have yet.
//
// Storage is a single JSON file beside the server. It survives Render's
// sleep; a redeploy starts it empty, and the next signed-in client repopulates
// it from its own local copy. Buckets are capped so one client cannot grow
// this into a database.

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	vaultMaxUserBytes = 512 * 1024
	vaultMaxUsers     = 4000
	vaultMaxMonths    = 48
)

type vaultEntry struct {
	UpdatedAt int64             `json:"updatedAt"`
	Buckets   json.RawMessage   `json:"buckets"`
}

var (
	vaultMu   sync.Mutex
	vaultData map[string]vaultEntry
	vaultFile string
)

func vaultLoad() {
	vaultFile = os.Getenv("VAULT_PATH")
	if vaultFile == "" {
		vaultFile = "vault.json"
	}
	vaultData = make(map[string]vaultEntry)
	raw, err := os.ReadFile(vaultFile)
	if err != nil {
		return
	}
	var loaded map[string]vaultEntry
	if json.Unmarshal(raw, &loaded) == nil && loaded != nil {
		vaultData = loaded
	}
}

func vaultSaveLocked() {
	raw, err := json.Marshal(vaultData)
	if err != nil {
		return
	}
	tmp := vaultFile + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		os.Rename(tmp, vaultFile)
	}
}

// handleVaultGet answers with the caller's stored buckets, 404 when there are
// none — a fresh install pulls an empty answer and simply keeps its own.
func handleVaultGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("userId")
	if !validVaultId(id) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_user_id"})
		return
	}
	vaultMu.Lock()
	entry, ok := vaultData[id]
	vaultMu.Unlock()
	if !ok {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": "empty_vault"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(entry.Buckets)
}

// handleVaultPut stores the body as the account's whole vault. The client
// sends its merged month buckets; a pull merges them into whatever the fresh
// install still has.
func handleVaultPut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("userId")
	if !validVaultId(id) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_user_id"})
		return
	}
	body := make([]byte, vaultMaxUserBytes+1)
	n, _ := r.Body.Read(body)
	body = body[:n]
	if n > vaultMaxUserBytes {
		jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]interface{}{"error": "vault_too_large"})
		return
	}
	var probe []json.RawMessage
	if json.Unmarshal(body, &probe) != nil || len(probe) > vaultMaxMonths {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_buckets"})
		return
	}

	vaultMu.Lock()
	if _, exists := vaultData[id]; !exists && len(vaultData) >= vaultMaxUsers {
		vaultMu.Unlock()
		jsonResponse(w, http.StatusInsufficientStorage, map[string]interface{}{"error": "vault_full"})
		return
	}
	vaultData[id] = vaultEntry{UpdatedAt: time.Now().UnixMilli(), Buckets: json.RawMessage(body)}
	vaultSaveLocked()
	vaultMu.Unlock()

	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true, "bytes": n})
}

// A vault key is the client-side hash of the login: hex, fixed length.
func validVaultId(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
