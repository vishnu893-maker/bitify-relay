package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// The status of a message as one person sees it: sent, delivered, read.
//
// The server owns this. A client asks "mark this conversation read up to
// here" and the server works out whose messages that covers and who is
// allowed to know that they were read. A client never sends a read receipt
// for somebody else's message.

// readReceiptsOn says whether this listener's read state is visible to
// others. A listener who turns it off is simply never reported as having
// read anything; the read itself is still remembered, so turning the switch
// back on shows the truth.
func readReceiptsOn(ctx context.Context, uid int64) bool {
	var on bool = true
	_ = msgDB.QueryRowContext(ctx, `SELECT show_read_receipts FROM social_profiles WHERE user_id=$1`, uid).Scan(&on)
	return on
}

// activityOn says whether this listener's typing and online state is
// visible. It is a separate switch from the read receipts: someone may be
// happy to say they have read a message without wanting to be watched
// typing.
func activityOn(ctx context.Context, uid int64) bool {
	var on bool = true
	_ = msgDB.QueryRowContext(ctx, `SELECT show_activity FROM social_profiles WHERE user_id=$1`, uid).Scan(&on)
	return on
}

// markDelivered records that the message reached this person's account. It
// is written when they connect, and when they are sent a message, so a
// message sent while they were offline is delivered the moment they are
// back.
func markDelivered(ctx context.Context, messageID, uid int64) {
	_, _ = msgDB.ExecContext(ctx,
		`INSERT INTO chat_message_status (message_id,user_id,delivered_at) VALUES ($1,$2,now())
		 ON CONFLICT (message_id,user_id) DO UPDATE SET delivered_at=COALESCE(chat_message_status.delivered_at, now())`,
		messageID, uid)
}

// markReadUpTo records that this person has read every message in the
// conversation up to and including [upTo] that somebody else sent.
//
// The rows are only written for messages addressed to this person: reading
// your own messages is not a thing, and a receipt for your own message would
// be a lie about the other end of the conversation.
func markReadUpTo(ctx context.Context, conv, uid, upTo int64) int64 {
	var n int64
	// Only the messages that have not been read yet are picked up. Reading
	// the same conversation twice therefore changes nothing and announces
	// nothing: a receipt is said once, not once per refetch.
	rows, err := msgDB.QueryContext(ctx,
		`SELECT m.id FROM chat_messages m
		 WHERE m.conv_id=$1 AND m.id<=$2 AND m.sender_id<>$3 AND m.deleted_at IS NULL
		   AND NOT EXISTS (
		       SELECT 1 FROM chat_message_status s
		       WHERE s.message_id=m.id AND s.user_id=$4 AND s.read_at IS NOT NULL)`,
		conv, upTo, uid, uid)
	if err != nil {
		return 0
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		_, _ = msgDB.ExecContext(ctx,
			`INSERT INTO chat_message_status (message_id,user_id,delivered_at,read_at) VALUES ($1,$2,now(),now())
			 ON CONFLICT (message_id,user_id) DO UPDATE SET read_at=COALESCE(chat_message_status.read_at, now()),
			   delivered_at=COALESCE(chat_message_status.delivered_at, now())`,
			id, uid)
		n++
	}
	return n
}

// statusOf says where one message stands for one reader.
//
// "sent" always holds: the server has it. "delivered" and "read" are only
// reported when the reader allows it. A reader with read receipts off is
// reported as "delivered" and no further, so nothing about their reading
// leaks.
func statusOf(ctx context.Context, messageID, reader int64) string {
	var delivered, read sql.NullTime
	err := msgDB.QueryRowContext(ctx,
		`SELECT delivered_at, read_at FROM chat_message_status WHERE message_id=$1 AND user_id=$2`,
		messageID, reader).Scan(&delivered, &read)
	if err != nil {
		return "sent"
	}
	if read.Valid && readReceiptsOn(ctx, reader) {
		return "read"
	}
	if delivered.Valid {
		return "delivered"
	}
	return "sent"
}

// statusForSender reports the state of a message the reader sent.
//
// It looks at the other members of the conversation and asks how far each
// of them has got. A member who has not connected has no delivery row and
// therefore has not been delivered to. A member who has turned their read
// receipts off counts as delivered and no further, so their reading is
// never exposed.
//
// In a group the state shown is the furthest anybody has got, which is the
// ordinary "seen" tick; the exact list of who has read it is the "Seen by"
// view, and it applies the same privacy rule.
func statusForSender(ctx context.Context, messageID, sender int64) string {
	var conv int64
	if msgDB.QueryRowContext(ctx, `SELECT conv_id FROM chat_messages WHERE id=$1`, messageID).Scan(&conv) != nil {
		return "sent"
	}
	rows, err := msgDB.QueryContext(ctx,
		`SELECT user_id FROM conversation_members WHERE conv_id=$1 AND user_id<>$2`, conv, sender)
	if err != nil {
		return "sent"
	}
	var members []int64
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil {
			members = append(members, u)
		}
	}
	rows.Close()
	if len(members) == 0 {
		return "sent"
	}
	best := "sent"
	for _, m := range members {
		switch statusOf(ctx, messageID, m) {
		case "read":
			return "read"
		case "delivered":
			best = "delivered"
		}
	}
	return best
}

// deliverConversation marks everything waiting for this person in this
// conversation as delivered. It is called when the socket opens, so a
// message sent while they were away reaches them the moment they are back.
//
// Delivery is about the device, not the account: a message is delivered
// once it has actually reached a connected client. That is why the row is
// written here, when the socket opens, and not when the message is sent.
func deliverConversation(ctx context.Context, conv, uid int64) {
	_, _ = msgDB.ExecContext(ctx,
		`INSERT INTO chat_message_status (message_id,user_id,delivered_at)
		 SELECT id,$2,now() FROM chat_messages
		 WHERE conv_id=$1 AND sender_id<>$2 AND deleted_at IS NULL
		 ON CONFLICT (message_id,user_id) DO UPDATE SET delivered_at=COALESCE(chat_message_status.delivered_at, now())`,
		conv, uid)
}

// handleMessageStatus: what the reader is allowed to know about a message.
//
// In a direct chat this is one other person's state. In a group it is the
// list of who has read it, which is what the "Seen by" view shows.
func handleMessageStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var conv, sender int64
	if msgDB.QueryRow(`SELECT conv_id, sender_id FROM chat_messages WHERE id=$1`, id).Scan(&conv, &sender) != nil {
		jsonError(w, http.StatusNotFound, "not_found", "No such message.")
		return
	}
	if role, _ := memberRole(r.Context(), conv, user.ID); role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	out := map[string]interface{}{"messageId": id}
	// The reader's own state of their own message is "sent".
	if sender == user.ID {
		out["status"] = "sent"
		jsonResponse(w, http.StatusOK, out)
		return
	}
	// In a direct chat, the status of the other person.
	var peers []int64
	for _, m := range convMemberIDs(r.Context(), conv) {
		if m != user.ID {
			peers = append(peers, m)
		}
	}
	seen := []interface{}{}
	delivered := []interface{}{}
	for _, p := range peers {
		st := statusOf(r.Context(), id, p)
		if st == "read" {
			if p, ok := loadProfileView(p, user.ID); ok {
				seen = append(seen, p)
			}
		} else if st == "delivered" {
			if p, ok := loadProfileView(p, user.ID); ok {
				delivered = append(delivered, p)
			}
		}
	}
	out["seenBy"] = seen
	out["deliveredTo"] = delivered
	// The strongest state anybody has reached, for a single tick mark.
	switch {
	case len(seen) > 0:
		out["status"] = "read"
	case len(delivered) > 0:
		out["status"] = "delivered"
	default:
		out["status"] = "sent"
	}
	jsonResponse(w, http.StatusOK, out)
}

// handleSeenBy lists who has read one message in a group.
//
// It is the same information the status endpoint carries, kept as its own
// route so the client can ask for it without knowing the message shape.
func handleSeenBy(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var conv int64
	if msgDB.QueryRow(`SELECT conv_id FROM chat_messages WHERE id=$1`, id).Scan(&conv) != nil {
		jsonError(w, http.StatusNotFound, "not_found", "No such message.")
		return
	}
	if role, _ := memberRole(r.Context(), conv, user.ID); role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	rows, err := msgDB.Query(
		`SELECT s.user_id FROM chat_message_status s WHERE s.message_id=$1 AND s.read_at IS NOT NULL`, id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Could not load read receipts.")
		return
	}
	defer rows.Close()
	seen := []interface{}{}
	for rows.Next() {
		var uid int64
		if rows.Scan(&uid) == nil && uid != user.ID {
			// Somebody who has turned their read receipts off is never on
			// this list: their reading is not exposed by asking for it.
			if !readReceiptsOn(r.Context(), uid) {
				continue
			}
			if p, ok := loadProfileView(uid, user.ID); ok {
				seen = append(seen, p)
			}
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"messageId": id, "seenBy": seen})
}

// handleForward sends a message on to other people.
//
// The content is carried by reference: the new message points at the
// original rather than copying the bytes. A voice note or a GIF therefore
// travels as the same stored file, and a restricted item is never
// re-hosted by forwarding it.
//
// The original must still be readable by the sender, and the new message
// is written into each chosen conversation as if they had sent it
// themselves.
func handleForward(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		// The conversations to forward into, named by conversation id.
		To []int64 `json:"to"`
		// People, named by their user id. The server finds their direct
		// chat, so the client does not have to.
		Users []int64 `json:"users"`
		// Groups, named by their conversation id. They are checked to be
		// groups, so a person id can never be mistaken for one.
		Groups []int64 `json:"groups"`
		// Optional text the sender wants to add above the forwarded item.
		Note string `json:"note"`
	}
	if !readBody(w, r, &in) {
		return
	}
	// Every destination is worked out as a conversation before anything is
	// sent, so a person and a group can never be confused for one another.
	var destinations []int64
	for _, dest := range in.To {
		destinations = append(destinations, dest)
	}
	for _, person := range in.Users {
		var direct int64
		lo, hi := pair(user.ID, person)
		key := strconv.FormatInt(lo, 10) + ":" + strconv.FormatInt(hi, 10)
		if msgDB.QueryRow(`SELECT id FROM conversations WHERE direct_key=$1`, key).Scan(&direct) == nil {
			destinations = append(destinations, direct)
		}
	}
	for _, group := range in.Groups {
		var kindOf string
		if msgDB.QueryRow(`SELECT kind FROM conversations WHERE id=$1`, kindOf).Scan(&kindOf) == nil && kindOf == "group" {
			destinations = append(destinations, group)
		}
	}
	if len(destinations) == 0 || len(destinations) > 50 {
		jsonError(w, http.StatusBadRequest, "bad_request", "Choose who to forward to.")
		return
	}
	var srcConv, srcSender int64
	var kind, body, payload string
	var replyTo int64
	var fwd bool
	var deleted sql.NullTime
	err := msgDB.QueryRow(
		`SELECT conv_id, sender_id, kind, body, payload, reply_to, forwarded, deleted_at
		 FROM chat_messages WHERE id=$1`, id).
		Scan(&srcConv, &srcSender, &kind, &body, &payload, &replyTo, &fwd, &deleted)
	if err != nil {
		jsonError(w, http.StatusNotFound, "not_found", "No such message.")
		return
	}
	if role, _ := memberRole(r.Context(), srcConv, user.ID); role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	if deleted.Valid {
		jsonError(w, http.StatusBadRequest, "bad_request", "That message was deleted.")
		return
	}
	sent := []interface{}{}
	for _, conv := range destinations {
		role, acc := memberRole(r.Context(), conv, user.ID)
		if role == "" || !acc {
			continue
		}
		// A Listen Together invite only travels while the room behind it is
		// still alive; an expired code is not worth sending.
		if kind == "session" {
			code := payloadField(payload, "code")
			if code == "" || !partyAlive(code) {
				continue
			}
		}
		// The words of the original are kept, and a note is put in front of
		// them rather than over them, so nothing the sender wrote is lost.
		note := strings.TrimSpace(in.Note)
		newBody := body
		if note != "" {
			if body == "" {
				newBody = note
			} else {
				newBody = note + "\n" + body
			}
		}
		var newID int64
		// The quote goes with it only when the quoted message is in the same
		// conversation; otherwise the bubble would point at something the
		// reader cannot see.
		newReplyTo := replyTo
		if newReplyTo != 0 {
			var replyConv int64
			if msgDB.QueryRow(`SELECT conv_id FROM chat_messages WHERE id=$1`, newReplyTo).Scan(&replyConv) != nil || replyConv != conv {
				newReplyTo = 0
			}
		}
		if msgDB.QueryRow(
			`INSERT INTO chat_messages (conv_id,sender_id,kind,body,payload,reply_to,forwarded,forwarded_from)
			 VALUES ($1,$2,$3,$4,$5,$6,TRUE,$7) RETURNING id`,
			conv, user.ID, kind, newBody, payload, newReplyTo, id).Scan(&newID) != nil {
			continue
		}
		_, _ = msgDB.Exec(`UPDATE conversations SET updated_at=now() WHERE id=$1`, conv)
		_, _ = msgDB.Exec(`UPDATE conversation_members SET last_read_id=$3 WHERE conv_id=$1 AND user_id=$2`, conv, user.ID, newID)
		msg := loadMessage(newID)
		for _, m := range convMemberIDs(r.Context(), conv) {
			if m == user.ID {
				continue
			}
			if _, accepted := memberRole(r.Context(), conv, m); accepted {
				hubPushUser(m, map[string]interface{}{"type": "message", "message": msg})
			}
		}
		sent = append(sent, msg)
	}
	if len(sent) == 0 {
		jsonError(w, http.StatusBadRequest, "bad_request", "Could not forward to anyone there.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"messages": sent})
}

// payloadField reads one field out of a JSON payload without failing the
// whole message over a missing key.
func payloadField(payload, field string) string {
	raw, err := payloadMap(payload)
	if err != nil {
		return ""
	}
	v, ok := raw[field]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// payloadMap parses a JSON object payload.
func payloadMap(payload string) (map[string]interface{}, error) {
	var out map[string]interface{}
	if payload == "" {
		return nil, sql.ErrNoRows
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// partyAlive says whether a Listen Together room is still worth joining.
//
// The party store is the one place that knows: a room that has ended or
// expired is not in it, and a room that is in it can still be joined. The
// invite card is only allowed to travel while this holds.
func partyAlive(code string) bool {
	if code == "" {
		return false
	}
	p, err := store.Get(code)
	if err != nil {
		return false
	}
	p.Lock()
	defer p.Unlock()
	return p.ToWire() != nil
}

// handleDeleteForMe hides a message from this person only.
//
// The message stays for everybody else. The row is not touched, so the
// conversation history on another device still has it.
func handleDeleteForMe(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var conv int64
	if msgDB.QueryRow(`SELECT conv_id FROM chat_messages WHERE id=$1`, id).Scan(&conv) != nil {
		jsonError(w, http.StatusNotFound, "not_found", "No such message.")
		return
	}
	if role, _ := memberRole(r.Context(), conv, user.ID); role == "" {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	_, _ = msgDB.Exec(`INSERT INTO chat_message_hides (message_id,user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, user.ID)
	hubPushConv(r.Context(), conv, user.ID, map[string]interface{}{"type": "message_hidden", "messageId": id, "user": user.ID})
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleTyping records that someone is typing, and tells the others.
//
// The state is a hint that expires: a row older than a few seconds is not
// read as "typing" any more, so a listener who walks away does not leave a
// permanent cursor in somebody else's header.
func handleTyping(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	conv, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Typing bool `json:"typing"`
	}
	if !readBody(w, r, &in) {
		return
	}
	role, acc := memberRole(r.Context(), conv, user.ID)
	if role == "" || !acc {
		jsonError(w, http.StatusForbidden, "forbidden", "Not your conversation.")
		return
	}
	if !in.Typing {
		_, _ = msgDB.Exec(`DELETE FROM chat_typing WHERE conv_id=$1 AND user_id=$2`, conv, user.ID)
		hubPushConv(r.Context(), conv, user.ID, map[string]interface{}{"type": "typing", "conv": conv, "user": user.ID, "typing": false})
		return
	}
	_, _ = msgDB.Exec(
		`INSERT INTO chat_typing (conv_id,user_id,last_updated) VALUES ($1,$2,now())
		 ON CONFLICT (conv_id,user_id) DO UPDATE SET last_updated=now()`, conv, user.ID)
	// A listener who has turned their activity status off is not announced
	// as typing.
	if !activityOn(r.Context(), user.ID) {
		return
	}
	hubPushConv(r.Context(), conv, user.ID, map[string]interface{}{"type": "typing", "conv": conv, "user": user.ID, "typing": true})
}

// handleMention marks the people named in a group message.
//
// The client sends the usernames it found in the body; the server checks
// each one is a member of the conversation before recording it, so a mention
// cannot be aimed at somebody who is not in the chat.
func handleMention(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Users []string `json:"users"`
	}
	if !readBody(w, r, &in) {
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
	members := map[string]bool{}
	for _, m := range convMemberIDs(r.Context(), conv) {
		var un string
		if msgDB.QueryRow(`SELECT username FROM social_profiles WHERE user_id=$1`, m).Scan(&un) == nil {
			members[strings.ToLower(un)] = true
		}
	}
	_, _ = msgDB.Exec(`DELETE FROM chat_mentions WHERE message_id=$1`, id)
	mentioned := []interface{}{}
	for _, u := range in.Users {
		un := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(u), "@"))
		if un == "" || !members[un] {
			continue
		}
		var uid int64
		if msgDB.QueryRow(`SELECT user_id FROM social_profiles WHERE lower(username)=$1`, un).Scan(&uid) != nil {
			continue
		}
		if uid == user.ID {
			continue
		}
		_, _ = msgDB.Exec(`INSERT INTO chat_mentions (message_id,user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, uid)
		mentioned = append(mentioned, uid)
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"mentions": mentioned})
}
