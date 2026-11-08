package main

// The account photo (2026-10-28): one picture per login, so changing it on
// the phone is changing it on the PC.
//
// Same shape as the vault (vault.go): the client hashes its login's ids, never
// sending the ids themselves, and gets back the small JSON blob it stored —
// here, a single avatar string. The avatar may be an https URL (the Google
// photo, or any picture the user pasted) or a data-URI jpeg the client picked
// from the device; both travel well and need no storage of their own.
//
// One file beside the server, one capped field, closed by the same hex-only
// key check the vault uses.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	profileMaxUsers  = 4000
	profileMaxBytes  = 192 * 1024 // the whole record, base64 picture included
	profileMaxAvatar = 180 * 1024
)

type profileRecord struct {
	UpdatedAt int64  `json:"updatedAt"`
	Avatar    string `json:"avatar"`
}

var (
	profileMu   sync.Mutex
	profileData map[string]profileRecord
	profileFile string
)

func profileLoad() {
	profileFile = os.Getenv("PROFILE_PATH")
	if profileFile == "" {
		profileFile = "profiles.json"
	}
	profileData = make(map[string]profileRecord)
	raw, err := os.ReadFile(profileFile)
	if err != nil {
		return
	}
	var loaded map[string]profileRecord
	if json.Unmarshal(raw, &loaded) == nil && loaded != nil {
		profileData = loaded
	}
}

func profileSaveLocked() {
	raw, err := json.Marshal(profileData)
	if err != nil {
		return
	}
	tmp := profileFile + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		os.Rename(tmp, profileFile)
	}
}

// handleProfileGet answers with what this account has stored, 404 when it has
// nothing — a device that has never chosen a picture reads an empty answer and
// keeps the Google thumbnail it already had.
func handleProfileGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("userId")
	if !validVaultId(id) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_user_id"})
		return
	}
	profileMu.Lock()
	record, ok := profileData[id]
	profileMu.Unlock()
	if !ok {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": "no_profile"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"avatar":    record.Avatar,
		"updatedAt": record.UpdatedAt,
	})
}

// handleProfilePut stores the body as the account's profile. The whole record
// is capped so one client cannot turn this into an image host: a data-URI
// photo has to arrive already downscaled, which is what the apps do anyway.
func handleProfilePut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("userId")
	if !validVaultId(id) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_user_id"})
		return
	}
	// Read directly rather than through decodeJSONBody: that helper enforces
	// the server's ordinary 16 KB JSON ceiling, and a downscaled photo rides
	// well past it. The cap here is the record's own, base64 and all.
	//
	// Read to the end, not once: a photo this size arrives in several chunks,
	// and a partial read truncated the JSON into an invalid_json rejection.
	raw, err := io.ReadAll(io.LimitReader(r.Body, profileMaxBytes+1))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "bad_body", "Could not read the body.")
		return
	}
	if len(raw) > profileMaxBytes {
		jsonError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Profile is too large.")
		return
	}
	var decoded struct {
		// Pointer field: an absent key means "not what this client is here to
		// change" and leaves the stored value alone; a present empty one
		// deliberately clears it. One device renaming must not blank the
		// picture another one chose.
		Avatar *string `json:"avatar"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		jsonError(w, http.StatusUnprocessableEntity, "invalid_json", "Malformed JSON body.")
		return
	}
	if decoded.Avatar != nil && !okAvatar(*decoded.Avatar) {
		jsonError(w, http.StatusUnprocessableEntity, "bad_avatar", "avatar must be an https URL or a data URI.")
		return
	}

	profileMu.Lock()
	if _, exists := profileData[id]; !exists && len(profileData) >= profileMaxUsers {
		profileMu.Unlock()
		jsonResponse(w, http.StatusInsufficientStorage, map[string]interface{}{"error": "profiles_full"})
		return
	}
	prev := profileData[id]
	updated := profileRecord{
		UpdatedAt: time.Now().UnixMilli(),
		Avatar:    prev.Avatar,
	}
	if decoded.Avatar != nil {
		updated.Avatar = *decoded.Avatar
	}
	profileData[id] = updated
	profileSaveLocked()
	profileMu.Unlock()

	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// The two shapes a phone or a PC actually sends: a real URL over https, or a
// data URI the client downscaled itself. Anything else would end up in an
// <img> slot that cannot render it, so it is refused at the door.
func okAvatar(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > profileMaxAvatar {
		return false
	}
	if len(value) >= 8 && value[:8] == "https://" {
		return true
	}
	return len(value) >= 15 && value[:5] == "data:" && containsComma(value)
}

func containsComma(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == ',' {
			return i > 5
		}
	}
	return false
}