package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Allowed upload types, checked against the bytes themselves (sniffed), not
// against the Content-Type the client claims.
var mediaLimits = map[string]int64{
	"image/jpeg": 5 << 20,
	"image/png":  5 << 20,
	"image/webp": 5 << 20,
	"audio/mp4":  3 << 20,
	"audio/mpeg": 3 << 20,
	"audio/ogg":  3 << 20,
	"audio/webm": 3 << 20,
	// The desktop records into a WAV container because Java Sound hands back
	// raw PCM; the same three-megabyte ceiling applies to it.
	"audio/wav": 3 << 20,
}

func sniffMime(b []byte) string {
	switch {
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8:
		return "image/jpeg"
	case len(b) > 8 && string(b[1:4]) == "PNG":
		return "image/png"
	case len(b) > 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) > 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "audio/wav"
	case len(b) > 12 && string(b[4:8]) == "ftyp":
		return "audio/mp4"
	case len(b) > 3 && string(b[0:3]) == "ID3", len(b) > 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0:
		return "audio/mpeg"
	case len(b) > 4 && string(b[0:4]) == "OggS":
		return "audio/ogg"
	case len(b) > 4 && b[0] == 0x1A && b[1] == 0x45 && b[2] == 0xDF && b[3] == 0xA3:
		return "audio/webm"
	}
	return ""
}

func handleMediaUpload(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	conv, _ := strconv.ParseInt(r.URL.Query().Get("conv"), 10, 64)
	if role, acc := memberRole(r.Context(), conv, user.ID); role == "" || !acc {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20+1)
	data, err := io.ReadAll(r.Body)
	if err != nil || len(data) == 0 {
		jsonError(w, http.StatusRequestEntityTooLarge, "too_large", "File is empty or too large.")
		return
	}
	mime := sniffMime(data)
	limit, allowed := mediaLimits[mime]
	if !allowed || int64(len(data)) > limit {
		jsonError(w, http.StatusUnsupportedMediaType, "bad_type", "That file type or size isn't allowed.")
		return
	}
	dur, _ := strconv.ParseInt(r.URL.Query().Get("durationMs"), 10, 64)
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	id := hex.EncodeToString(raw)
	if _, err := msgDB.Exec(`INSERT INTO chat_media (id,uploader,conv_id,mime,size,duration_ms,data) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, user.ID, conv, mime, len(data), dur, data); err != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Upload failed.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"mediaId": id, "mime": mime, "size": len(data)})
}

// handleMediaGet serves a file only to members of the conversation it was
// uploaded to; the id alone is never enough.
func handleMediaGet(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		http.NotFound(w, r)
		return
	}
	var conv int64
	var mime string
	var data []byte
	if msgDB.QueryRow(`SELECT conv_id, mime, data FROM chat_media WHERE id=$1`, id).Scan(&conv, &mime, &data) != nil {
		http.NotFound(w, r)
		return
	}
	if role, _ := memberRole(r.Context(), conv, user.ID); role == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// ---- Music activity ----

func handleActivityPost(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	var in struct {
		Kind    string `json:"kind"`
		Payload string `json:"payload"`
	}
	valid := map[string]bool{"listening": true, "artist": true, "playlist": true, "album": true, "session": true}
	if !readBody(w, r, &in) || !valid[in.Kind] || len(in.Payload) > 4000 {
		jsonError(w, http.StatusBadRequest, "bad_request", "Invalid activity.")
		return
	}
	var vis = "friends"
	_ = msgDB.QueryRow(`SELECT activity_visibility FROM social_profiles WHERE user_id=$1`, user.ID).Scan(&vis)
	if vis == "nobody" {
		jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true, "stored": false})
		return
	}
	if in.Kind == "listening" {
		_, _ = msgDB.Exec(`DELETE FROM music_activity WHERE user_id=$1 AND kind='listening'`, user.ID)
	}
	_, _ = msgDB.Exec(`INSERT INTO music_activity (user_id,kind,payload) VALUES ($1,$2,$3)`, user.ID, in.Kind, in.Payload)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true, "stored": true})
}

// handleActivityFeed returns only what each author's visibility allows to
// *this* viewer: friends-only items need an accepted friendship.
func handleActivityFeed(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	rows, err := msgDB.Query(`
	SELECT a.id, a.user_id, a.kind, a.payload, extract(epoch from a.created_at)::bigint*1000
	FROM music_activity a
	JOIN friendships f ON f.status='accepted' AND ((f.user_lo=$1 AND f.user_hi=a.user_id) OR (f.user_hi=$1 AND f.user_lo=a.user_id))
	LEFT JOIN social_profiles p ON p.user_id=a.user_id
	WHERE NOT a.hidden AND COALESCE(p.activity_visibility,'friends') IN ('friends','everyone')
	AND NOT EXISTS (SELECT 1 FROM social_blocks b WHERE (b.blocker=$1 AND b.blocked=a.user_id) OR (b.blocker=a.user_id AND b.blocked=$1))
	AND NOT EXISTS (SELECT 1 FROM music_activity_hides h WHERE h.activity_id=a.id AND h.user_id=$1)
	ORDER BY a.id DESC LIMIT 60`, user.ID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Could not load activity.")
		return
	}
	defer rows.Close()
	out := []interface{}{}
	for rows.Next() {
		var id, uid, at int64
		var kind, payload string
		if rows.Scan(&id, &uid, &kind, &payload, &at) == nil {
			p, _ := loadProfileView(uid, user.ID)
			out = append(out, map[string]interface{}{"id": id, "user": p, "kind": kind, "payload": payload, "createdAt": at})
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"activity": out})
}

// handleActivityHide takes one item out of the caller's own feed.
//
// It has to work for somebody else's post too: hiding an item in your feed
// says nothing about the person who posted it. So the hide is recorded against
// the viewer in music_activity_hides, and only your own posts are hidden
// outright.
func handleActivityHide(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id == 0 {
		jsonError(w, http.StatusBadRequest, "bad_request", "Which item?")
		return
	}
	// The viewer may only hide an item they can actually see.
	var visible bool
	_ = msgDB.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM music_activity a
			JOIN friendships f ON f.status='accepted' AND ((f.user_lo=$1 AND f.user_hi=a.user_id) OR (f.user_hi=$1 AND f.user_lo=a.user_id))
			LEFT JOIN social_profiles p ON p.user_id=a.user_id
			WHERE a.id=$2 AND NOT a.hidden
			AND COALESCE(p.activity_visibility,'friends') IN ('friends','everyone')
			AND NOT EXISTS (SELECT 1 FROM social_blocks b WHERE (b.blocker=$1 AND b.blocked=a.user_id) OR (b.blocker=a.user_id AND b.blocked=$1))
		)`, user.ID, id).Scan(&visible)
	if !visible {
		jsonError(w, http.StatusNotFound, "not_found", "That item is not in your feed.")
		return
	}
	_, _ = msgDB.Exec(`INSERT INTO music_activity_hides (activity_id,user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, user.ID)
	// Your own post disappears from everybody's feed, not just yours.
	_, _ = msgDB.Exec(`UPDATE music_activity SET hidden=TRUE WHERE id=$1 AND user_id=$2`, id, user.ID)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func registerMessagesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/social/me", handleSocialMe)
	mux.HandleFunc("PUT /api/social/me", handleSocialMe)
	mux.HandleFunc("GET /api/social/privacy", handleSocialPrivacy)
	mux.HandleFunc("PUT /api/social/privacy", handleSocialPrivacy)
	mux.HandleFunc("GET /api/social/notifications", handleSocialNotifications)
	mux.HandleFunc("PUT /api/social/notifications", handleSocialNotifications)
	mux.HandleFunc("GET /api/social/users", handleUserSearch)
	mux.HandleFunc("GET /api/social/users/{id}", handleUserGet)
	mux.HandleFunc("GET /api/social/friends", handleFriendsList)
	mux.HandleFunc("POST /api/social/users/{id}/{action}", handleFriendAction)
	mux.HandleFunc("POST /api/social/report", handleReport)
	mux.HandleFunc("GET /api/social/activity", handleActivityFeed)
	mux.HandleFunc("POST /api/social/activity", handleActivityPost)
	mux.HandleFunc("POST /api/social/activity/{id}/hide", handleActivityHide)

	mux.HandleFunc("GET /api/messages/conversations", handleConvList)
	mux.HandleFunc("POST /api/messages/direct", handleConvOpenDirect)
	mux.HandleFunc("GET /api/messages/conversations/{id}", handleConversationGet)
	mux.HandleFunc("PATCH /api/messages/conversations/{id}", handleConversationUpdate)
	mux.HandleFunc("POST /api/messages/conversations/{id}/mute", handleConversationMute)
	mux.HandleFunc("POST /api/messages/groups", handleGroupCreate)
	mux.HandleFunc("POST /api/messages/groups/{id}/members/{uid}/{action}", handleGroupMember)
	mux.HandleFunc("GET /api/messages/conversations/{id}/messages", handleMessagesList)
	mux.HandleFunc("POST /api/messages/conversations/{id}/messages", handleMessageSend)
	mux.HandleFunc("POST /api/messages/conversations/{id}/read", handleRead)
	mux.HandleFunc("PATCH /api/messages/messages/{id}", handleMessageEdit)
	mux.HandleFunc("DELETE /api/messages/messages/{id}", handleMessageEdit)
	mux.HandleFunc("POST /api/messages/messages/{id}/react", handleReaction)
	mux.HandleFunc("GET /api/messages/unread", handleUnreadTotal)
	mux.HandleFunc("POST /api/messages/media", handleMediaUpload)
	mux.HandleFunc("GET /api/messages/media/{id}", handleMediaGet)
	mux.HandleFunc("GET /ws/messages", handleMessagesSocket)
}
