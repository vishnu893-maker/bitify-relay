package main

import (
	"net/http"
	"strconv"
)

// Group and chat detail: who is in it, what it is called, and what this user
// is allowed to do. The list endpoint carries enough to draw the sidebar, but
// a group's own screen needs its members, which is what this adds.

func memberRowsOf(conv int64) []map[string]interface{} {
	rows, err := msgDB.Query(`SELECT user_id, role, accepted FROM conversation_members WHERE conv_id=$1 ORDER BY joined_at`, conv)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var uid int64
		var role string
		var accepted bool
		if rows.Scan(&uid, &role, &accepted) != nil {
			continue
		}
		out = append(out, map[string]interface{}{"id": uid, "role": role, "accepted": accepted})
	}
	return out
}

func handleConversationGet(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	conv, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	role, accepted := memberRole(r.Context(), conv, user.ID)
	if role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	var kind, title, desc, avatar string
	if msgDB.QueryRow(`SELECT kind, title, description, avatar_url FROM conversations WHERE id=$1`, conv).
		Scan(&kind, &title, &desc, &avatar) != nil {
		jsonError(w, http.StatusNotFound, "not_found", "No such conversation.")
		return
	}
	members := []map[string]interface{}{}
	for _, m := range memberRowsOf(conv) {
		uid := m["id"].(int64)
		if p, ok := loadProfileView(uid, user.ID); ok {
			m["user"] = p
		}
		members = append(members, m)
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"id": conv, "kind": kind, "title": title, "description": desc, "avatarUrl": avatar,
		"role": role, "accepted": accepted, "members": members,
	})
}

// handleConversationUpdate renames a group, changes its description or
// picture. Admin only, and groups only: a 1:1 chat has no title of its own.
func handleConversationUpdate(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	conv, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	role, accepted := memberRole(r.Context(), conv, user.ID)
	var kind string
	_ = msgDB.QueryRow(`SELECT kind FROM conversations WHERE id=$1`, conv).Scan(&kind)
	if role == "" || !accepted || kind != "group" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not allowed.")
		return
	}
	if role != "admin" {
		jsonError(w, http.StatusForbidden, "forbidden", "Admins only.")
		return
	}
	var in struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		AvatarURL   *string `json:"avatarUrl"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if in.Title != nil {
		t := clip(*in.Title, 60)
		if t == "" {
			jsonError(w, http.StatusBadRequest, "bad_request", "A group needs a name.")
			return
		}
		_, _ = msgDB.Exec(`UPDATE conversations SET title=$2, updated_at=now() WHERE id=$1`, conv, t)
	}
	if in.Description != nil {
		_, _ = msgDB.Exec(`UPDATE conversations SET description=$2 WHERE id=$1`, conv, clip(*in.Description, 200))
	}
	if in.AvatarURL != nil {
		_, _ = msgDB.Exec(`UPDATE conversations SET avatar_url=$2 WHERE id=$1`, conv, clip(*in.AvatarURL, 300))
	}
	for _, m := range convMemberIDs(r.Context(), conv) {
		if m != user.ID {
			hubPushUser(m, map[string]interface{}{"type": "group_update", "conv": conv})
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleConversationMute is per member, so a muted group stays quiet for one
// person without changing anything for anyone else.
func handleConversationMute(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	conv, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if role, _ := memberRole(r.Context(), conv, user.ID); role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	var in struct {
		Muted bool `json:"muted"`
	}
	if !readBody(w, r, &in) {
		return
	}
	_, _ = msgDB.Exec(`UPDATE conversation_members SET muted=$3 WHERE conv_id=$1 AND user_id=$2`, conv, user.ID, in.Muted)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true, "muted": in.Muted})
}
