package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The kinds a message may be sent as. Photos and video are deliberately
// absent: the media policy is voice notes and GIFs, plus music cards.
var validKinds = map[string]bool{"text": true, "music": true, "gif": true, "voice": true, "session": true}

// handleConvOpenDirect finds or creates the 1:1 chat with another user. A
// non-friend gets a message *request*: the chat exists but the recipient has
// not accepted it, so nothing reaches them until they do.
func handleConvOpenDirect(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	var in struct {
		UserID int64 `json:"userId"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if in.UserID == 0 || in.UserID == user.ID || isBlockedEitherWay(r.Context(), user.ID, in.UserID) {
		jsonError(w, http.StatusForbidden, "blocked", "You can't message this user.")
		return
	}
	lo, hi := pair(user.ID, in.UserID)
	key := fmt.Sprintf("%d:%d", lo, hi)
	var id int64
	err := msgDB.QueryRow(`SELECT id FROM conversations WHERE direct_key=$1`, key).Scan(&id)
	if err == sql.ErrNoRows {
		tx, _ := msgDB.Begin()
		if tx.QueryRow(`INSERT INTO conversations (kind, created_by, direct_key) VALUES ('direct',$1,$2) RETURNING id`, user.ID, key).Scan(&id) != nil {
			_ = tx.Rollback()
			jsonError(w, http.StatusInternalServerError, "db", "Could not open chat.")
			return
		}
		accepted := areFriends(r.Context(), user.ID, in.UserID)
		_, _ = tx.Exec(`INSERT INTO conversation_members (conv_id,user_id,role,accepted) VALUES ($1,$2,'member',TRUE)`, id, user.ID)
		_, _ = tx.Exec(`INSERT INTO conversation_members (conv_id,user_id,role,accepted) VALUES ($1,$2,'member',$3)`, id, in.UserID, accepted)
		_ = tx.Commit()
		if !accepted {
			hubPushUser(in.UserID, map[string]interface{}{"type": "message_request", "conv": id, "from": user.ID})
		}
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Could not open chat.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"conversationId": id})
}

func handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	var in struct {
		Title       string  `json:"title"`
		Description string  `json:"description"`
		AvatarURL   string  `json:"avatarUrl"`
		Members     []int64 `json:"members"`
	}
	if !readBody(w, r, &in) {
		return
	}
	title := clip(in.Title, 60)
	if title == "" || len(in.Members) > 99 {
		jsonError(w, http.StatusBadRequest, "bad_request", "A group needs a name and fewer than 100 members.")
		return
	}
	var id int64
	if msgDB.QueryRow(`INSERT INTO conversations (kind,title,description,avatar_url,created_by) VALUES ('group',$1,$2,$3,$4) RETURNING id`,
		title, clip(in.Description, 200), clip(in.AvatarURL, 300), user.ID).Scan(&id) != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Could not create group.")
		return
	}
	_, _ = msgDB.Exec(`INSERT INTO conversation_members (conv_id,user_id,role) VALUES ($1,$2,'admin')`, id, user.ID)
	for _, m := range in.Members {
		if m == user.ID || isBlockedEitherWay(r.Context(), user.ID, m) || !areFriends(r.Context(), user.ID, m) {
			continue
		}
		_, _ = msgDB.Exec(`INSERT INTO conversation_members (conv_id,user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, m)
		hubPushUser(m, map[string]interface{}{"type": "group_invite", "conv": id, "title": title})
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"conversationId": id})
}

// handleGroupMember: admin-only add/remove/promote; anyone may leave.
func handleGroupMember(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	conv, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	target, _ := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	action := r.PathValue("action")
	role, acc := memberRole(r.Context(), conv, user.ID)
	var kind string
	_ = msgDB.QueryRow(`SELECT kind FROM conversations WHERE id=$1`, conv).Scan(&kind)
	if role == "" || !acc || kind != "group" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not allowed.")
		return
	}
	if action == "leave" && target == user.ID {
		_, _ = msgDB.Exec(`DELETE FROM conversation_members WHERE conv_id=$1 AND user_id=$2`, conv, user.ID)
		jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
		return
	}
	if role != "admin" {
		jsonError(w, http.StatusForbidden, "forbidden", "Admins only.")
		return
	}
	switch action {
	case "add":
		if isBlockedEitherWay(r.Context(), user.ID, target) {
			jsonError(w, http.StatusForbidden, "blocked", "You can't add this user.")
			return
		}
		_, _ = msgDB.Exec(`INSERT INTO conversation_members (conv_id,user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, conv, target)
		hubPushUser(target, map[string]interface{}{"type": "group_invite", "conv": conv})
	case "remove":
		_, _ = msgDB.Exec(`DELETE FROM conversation_members WHERE conv_id=$1 AND user_id=$2`, conv, target)
	case "promote":
		_, _ = msgDB.Exec(`UPDATE conversation_members SET role='admin' WHERE conv_id=$1 AND user_id=$2`, conv, target)
	default:
		jsonError(w, http.StatusBadRequest, "bad_request", "Unknown action.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleConvList: the sidebar. filter = all|direct|group|requests.
func handleConvList(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	filter := r.URL.Query().Get("filter")
	rows, err := msgDB.Query(`
	SELECT c.id, c.kind, c.title, c.avatar_url, c.description, m.accepted, m.muted, m.role,
	  CAST(EXTRACT(EPOCH FROM c.updated_at)*1000 AS BIGINT),
	  COALESCE((SELECT max(id) FROM chat_messages WHERE conv_id=c.id AND deleted_at IS NULL),0) AS last_id,
	  (SELECT count(*) FROM chat_messages x WHERE x.conv_id=c.id AND x.id>m.last_read_id AND x.sender_id<>$1 AND x.deleted_at IS NULL) AS unread,
	  COALESCE((SELECT user_id FROM conversation_members WHERE conv_id=c.id AND user_id<>$1 LIMIT 1),0) AS peer
	FROM conversations c JOIN conversation_members m ON m.conv_id=c.id AND m.user_id=$1
	ORDER BY c.updated_at DESC LIMIT 200`, user.ID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Could not load chats.")
		return
	}
	type item struct {
		ID, LastID, Unread, Peer, UpdatedAt int64
		Kind, Title, Avatar, Desc, Rl       string
		Accepted, Muted                     bool
	}
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.ID, &it.Kind, &it.Title, &it.Avatar, &it.Desc, &it.Accepted, &it.Muted, &it.Rl, &it.UpdatedAt, &it.LastID, &it.Unread, &it.Peer) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	out := []interface{}{}
	for _, it := range items {
		isRequest := !it.Accepted
		if filter == "requests" && !isRequest || filter != "requests" && isRequest {
			continue
		}
		if filter == "direct" && it.Kind != "direct" || filter == "group" && it.Kind != "group" {
			continue
		}
		entry := map[string]interface{}{
			"id": it.ID, "kind": it.Kind, "title": it.Title, "avatarUrl": it.Avatar,
			"description": it.Desc, "muted": it.Muted, "role": it.Rl, "unread": it.Unread,
			"request": isRequest, "updatedAt": it.UpdatedAt,
		}
		if it.Kind == "direct" {
			if isBlockedEitherWay(r.Context(), user.ID, it.Peer) {
				continue
			}
			if p, ok := loadProfileView(it.Peer, user.ID); ok {
				entry["peer"] = p
			}
		}
		if it.LastID > 0 {
			entry["last"] = loadMessage(it.LastID)
		}
		out = append(out, entry)
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"conversations": out})
}

func loadMessage(id int64) map[string]interface{} {
	var conv, sender, reply int64
	var kind, body, payload string
	var fwd bool
	var created int64
	var edited, deleted sql.NullTime
	err := msgDB.QueryRow(`SELECT conv_id, sender_id, kind, body, payload, reply_to, forwarded,
		extract(epoch from created_at)::bigint*1000, edited_at, deleted_at FROM chat_messages WHERE id=$1`, id).
		Scan(&conv, &sender, &kind, &body, &payload, &reply, &fwd, &created, &edited, &deleted)
	if err != nil {
		return nil
	}
	m := map[string]interface{}{
		"id": id, "conv": conv, "sender": sender, "kind": kind, "replyTo": reply,
		"forwarded": fwd, "createdAt": created, "edited": edited.Valid,
	}
	if deleted.Valid {
		m["deleted"] = true
		m["body"], m["payload"] = "", ""
	} else {
		m["body"], m["payload"] = body, payload
	}
	rr, err := msgDB.Query(`SELECT emoji, user_id FROM chat_reactions WHERE message_id=$1`, id)
	reactions := map[string][]int64{}
	if err == nil {
		for rr.Next() {
			var e string
			var u int64
			if rr.Scan(&e, &u) == nil {
				reactions[e] = append(reactions[e], u)
			}
		}
		rr.Close()
	}
	m["reactions"] = reactions
	return m
}

func handleMessagesList(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	conv, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if role, _ := memberRole(r.Context(), conv, user.ID); role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if before == 0 {
		before = 1 << 62
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 60 {
		limit = 40
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var rows *sql.Rows
	var err error
	if q != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
		rows, err = msgDB.Query(`SELECT id FROM chat_messages WHERE conv_id=$1 AND id<$2 AND deleted_at IS NULL
			AND kind IN ('text','music') AND body ILIKE $4 ORDER BY id DESC LIMIT $3`, conv, before, limit, "%"+esc+"%")
	} else {
		rows, err = msgDB.Query(`SELECT id FROM chat_messages WHERE conv_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3`, conv, before, limit)
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Could not load messages.")
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	out := []interface{}{}
	for _, id := range ids {
		if m := loadMessage(id); m != nil {
			out = append(out, m)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"messages": out, "hasMore": len(ids) == limit})
}

func handleMessageSend(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	conv, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	role, acc := memberRole(r.Context(), conv, user.ID)
	if role == "" || !acc {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	var in struct {
		Kind      string `json:"kind"`
		Body      string `json:"body"`
		Payload   string `json:"payload"`
		ReplyTo   int64  `json:"replyTo"`
		Forwarded bool   `json:"forwarded"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if in.Kind == "" {
		in.Kind = "text"
	}
	if !validKinds[in.Kind] || len(in.Body) > 4000 || len(in.Payload) > 8000 ||
		(in.Kind == "text" && strings.TrimSpace(in.Body) == "") || (in.Kind != "text" && in.Payload == "") {
		jsonError(w, http.StatusBadRequest, "bad_request", "Invalid message.")
		return
	}
	// A media message has to point at a file this conversation actually holds,
	// and that file has to be the kind the message says it is.
	if in.Kind == "gif" || in.Kind == "voice" {
		if !mediaPayloadOK(r.Context(), conv, in.Kind, in.Payload) {
			jsonError(w, http.StatusBadRequest, "bad_request", "That media doesn't belong here.")
			return
		}
	}
	var kind string
	_ = msgDB.QueryRow(`SELECT kind FROM conversations WHERE id=$1`, conv).Scan(&kind)
	if kind == "direct" {
		for _, m := range convMemberIDs(r.Context(), conv) {
			if m != user.ID && isBlockedEitherWay(r.Context(), user.ID, m) {
				jsonError(w, http.StatusForbidden, "blocked", "You can't message this user.")
				return
			}
		}
	}
	var id int64
	if msgDB.QueryRow(`INSERT INTO chat_messages (conv_id,sender_id,kind,body,payload,reply_to,forwarded)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, conv, user.ID, in.Kind, in.Body, in.Payload, in.ReplyTo, in.Forwarded).Scan(&id) != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Could not send.")
		return
	}
	_, _ = msgDB.Exec(`UPDATE conversations SET updated_at=now() WHERE id=$1`, conv)
	_, _ = msgDB.Exec(`UPDATE conversation_members SET last_read_id=$3 WHERE conv_id=$1 AND user_id=$2`, conv, user.ID, id)
	msg := loadMessage(id)
	for _, m := range convMemberIDs(r.Context(), conv) {
		if m == user.ID {
			continue
		}
		if _, accepted := memberRole(r.Context(), conv, m); accepted {
			hubPushUser(m, map[string]interface{}{"type": "message", "message": msg})
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": msg})
}

// handleMessageEdit: PATCH edits text (sender only); DELETE soft-deletes
// (sender, or a group admin).
func handleMessageEdit(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var conv, sender int64
	if msgDB.QueryRow(`SELECT conv_id, sender_id FROM chat_messages WHERE id=$1`, id).Scan(&conv, &sender) != nil {
		jsonError(w, http.StatusNotFound, "not_found", "No such message.")
		return
	}
	role, _ := memberRole(r.Context(), conv, user.ID)
	if role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	if r.Method == http.MethodDelete {
		if sender != user.ID && role != "admin" {
			jsonError(w, http.StatusForbidden, "forbidden", "Not yours to delete.")
			return
		}
		_, _ = msgDB.Exec(`UPDATE chat_messages SET deleted_at=now(), body='', payload='' WHERE id=$1`, id)
	} else {
		var in struct {
			Body string `json:"body"`
		}
		if sender != user.ID || !readBody(w, r, &in) || strings.TrimSpace(in.Body) == "" || len(in.Body) > 4000 {
			jsonError(w, http.StatusForbidden, "forbidden", "You can only edit your own text.")
			return
		}
		_, _ = msgDB.Exec(`UPDATE chat_messages SET body=$2, edited_at=now() WHERE id=$1 AND kind='text' AND deleted_at IS NULL`, id, in.Body)
	}
	msg := loadMessage(id)
	hubPushConv(r.Context(), conv, 0, map[string]interface{}{"type": "message_update", "message": msg})
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": msg})
}

func handleReaction(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Emoji string `json:"emoji"`
	}
	if !readBody(w, r, &in) || in.Emoji == "" || len([]rune(in.Emoji)) > 8 {
		jsonError(w, http.StatusBadRequest, "bad_request", "Invalid reaction.")
		return
	}
	var conv int64
	if msgDB.QueryRow(`SELECT conv_id FROM chat_messages WHERE id=$1`, id).Scan(&conv) != nil {
		jsonError(w, http.StatusNotFound, "not_found", "No such message.")
		return
	}
	if role, _ := memberRole(r.Context(), conv, user.ID); role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	res, _ := msgDB.Exec(`DELETE FROM chat_reactions WHERE message_id=$1 AND user_id=$2 AND emoji=$3`, id, user.ID, in.Emoji)
	if n, _ := res.RowsAffected(); n == 0 {
		_, _ = msgDB.Exec(`INSERT INTO chat_reactions (message_id,user_id,emoji) VALUES ($1,$2,$3)`, id, user.ID, in.Emoji)
	}
	msg := loadMessage(id)
	hubPushConv(r.Context(), conv, 0, map[string]interface{}{"type": "message_update", "message": msg})
	jsonResponse(w, http.StatusOK, map[string]interface{}{"message": msg})
}

// handleRead marks a conversation read up to a message and sends the receipt.
// Accepting a request is the same call with accept=true.
func handleRead(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	conv, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		UpTo   int64 `json:"upTo"`
		Accept bool  `json:"accept"`
	}
	if !readBody(w, r, &in) {
		return
	}
	role, _ := memberRole(r.Context(), conv, user.ID)
	if role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	if in.Accept {
		_, _ = msgDB.Exec(`UPDATE conversation_members SET accepted=TRUE WHERE conv_id=$1 AND user_id=$2`, conv, user.ID)
	}
	_, _ = msgDB.Exec(`UPDATE conversation_members SET last_read_id=GREATEST(last_read_id,$3) WHERE conv_id=$1 AND user_id=$2`, conv, user.ID, in.UpTo)
	hubPushConv(r.Context(), conv, user.ID, map[string]interface{}{"type": "read", "conv": conv, "user": user.ID, "upTo": in.UpTo})
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func handleUnreadTotal(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	var n int64
	_ = msgDB.QueryRow(`SELECT count(*) FROM chat_messages x JOIN conversation_members m ON m.conv_id=x.conv_id AND m.user_id=$1
		WHERE m.accepted AND NOT m.muted AND x.id>m.last_read_id AND x.sender_id<>$1 AND x.deleted_at IS NULL`, user.ID).Scan(&n)
	var req int64
	_ = msgDB.QueryRow(`SELECT count(*) FROM conversation_members WHERE user_id=$1 AND NOT accepted`, user.ID).Scan(&req)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"unread": n, "requests": req})
}
