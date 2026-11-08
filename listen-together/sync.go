package main

// Account sync (2026-11-08): the app's own data — playlists, and the Discord
// and Spotify credentials — carried between devices under one login.
//
// The server is deliberately dumb here. It stores the document the client
// built and hands it back; merging happens on the client, because only the
// client knows which playlist it edited and when. The server's job is to keep
// exactly one copy per account and to refuse a write built on a stale
// revision, so two devices cannot silently overwrite each other.
//
// The Discord token and the Spotify cookie inside the document are sealed with
// AES-GCM under SYNC_SECRET before they reach the database. The key lives in
// the environment, so a database dump on its own does not hand out anyone's
// credentials. With SYNC_SECRET unset the document is stored as written, which
// is the right default for a laptop run and a bad one for production.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"

	"net/http"

	"github.com/KabirSinghBhatia/BitChord/backend/config"
)

const (
	// A document is playlists plus a handful of credentials. 512 KiB is two
	// orders of magnitude more than a heavy listener needs and still small
	// enough that one account cannot fill the database.
	syncMaxDocBytes = 512 * 1024

	// Markers so a document sealed today can still be read after the key is
	// rotated: only the prefix decides how to open it.
	syncSealedPrefix = "enc:v1:"
)

// syncSealed reports whether the stored document is ciphertext.
func syncSealed(stored string) bool { return strings.HasPrefix(stored, syncSealedPrefix) }

// syncSeal encrypts a document for storage. No secret means no encryption,
// which the caller is expected to have warned about at boot.
func syncSeal(plain string) (string, error) {
	if config.SyncSecret == "" {
		return plain, nil
	}
	block, err := aes.NewCipher(syncKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return syncSealedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// syncOpen reverses syncSeal. A document written before a secret was configured
// is plaintext and passes through untouched.
func syncOpen(stored string) (string, error) {
	if !syncSealed(stored) {
		return stored, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, syncSealedPrefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(syncKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", io.ErrUnexpectedEOF
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// syncKey is the AES key: the configured secret, hashed to the one length AES
// accepts. Any string works as SYNC_SECRET, but changing it makes every stored
// document unreadable, so it is a key, not a setting.
func syncKey() []byte {
	sum := sha256.Sum256([]byte(config.SyncSecret))
	return sum[:]
}

// syncPlaylistCount is what the admin panel shows beside an account. Counting
// here, while the document is still plaintext, means the panel never has to
// decrypt anything.
func syncPlaylistCount(doc json.RawMessage) int {
	var shape struct {
		Playlists []json.RawMessage `json:"playlists"`
	}
	if err := json.Unmarshal(doc, &shape); err != nil {
		return 0
	}
	return len(shape.Playlists)
}

// syncDocView is one account's stored document as the client sees it.
func syncDocView(stored SyncDoc) (map[string]interface{}, error) {
	plain, err := syncOpen(stored.Doc)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"rev":       stored.Rev,
		"updatedAt": stored.UpdatedAt.UnixMilli(),
		"doc":       json.RawMessage(plain),
	}, nil
}

func handleSyncGet(w http.ResponseWriter, r *http.Request) {
	if !accountsReady(w) {
		return
	}
	user, _, ok := currentUser(r.Context(), r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
		return
	}
	stored, found, err := accountStore.SyncGet(r.Context(), user.ID)
	if err != nil {
		log.Printf("sync: read failed: %v", err)
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not read your data.")
		return
	}
	if !found {
		// A brand-new account has nothing yet; that is not an error, it is the
		// signal for the client to push its first copy.
		jsonResponse(w, http.StatusOK, map[string]interface{}{"rev": 0, "doc": nil})
		return
	}
	view, err := syncDocView(stored)
	if err != nil {
		log.Printf("sync: open failed: %v", err)
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not read your data.")
		return
	}
	jsonResponse(w, http.StatusOK, view)
}

func handleSyncPut(w http.ResponseWriter, r *http.Request) {
	if !accountsReady(w) {
		return
	}
	user, _, ok := currentUser(r.Context(), r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
		return
	}
	var body struct {
		BaseRev int64           `json:"baseRev"`
		Doc     json.RawMessage `json:"doc"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if len(body.Doc) == 0 || len(body.Doc) > syncMaxDocBytes {
		jsonError(w, http.StatusRequestEntityTooLarge, "sync_too_large", "That is too much data to sync.")
		return
	}
	if body.Doc[0] != '{' {
		jsonError(w, http.StatusUnprocessableEntity, "bad_sync_doc", "The sync document must be an object.")
		return
	}
	if body.BaseRev < 0 {
		body.BaseRev = 0
	}
	sealed, err := syncSeal(string(body.Doc))
	if err != nil {
		log.Printf("sync: seal failed: %v", err)
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not store your data.")
		return
	}
	stored, ok, err := accountStore.SyncPut(
		r.Context(),
		user.ID,
		sealed,
		syncPlaylistCount(body.Doc),
		body.BaseRev,
		time.Now(),
	)
	if err != nil {
		log.Printf("sync: write failed: %v", err)
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not store your data.")
		return
	}
	if !ok {
		// Another device wrote first. Hand back what is there so the client can
		// merge and retry instead of losing either side.
		current, found, readErr := accountStore.SyncGet(r.Context(), user.ID)
		if readErr != nil || !found {
			jsonError(w, http.StatusConflict, "sync_conflict", "Another device saved first.")
			return
		}
		view, viewErr := syncDocView(current)
		if viewErr != nil {
			jsonError(w, http.StatusConflict, "sync_conflict", "Another device saved first.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "sync_conflict",
			"message": "Another device saved first.",
			"rev":     view["rev"],
			"doc":     view["doc"],
		})
		return
	}
	view, err := syncDocView(stored)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "server_error", "Could not store your data.")
		return
	}
	jsonResponse(w, http.StatusOK, view)
}
