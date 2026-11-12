package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The messaging hub: one live socket set per user. REST writes the data; the
// hub only pushes events (new message, edit, reaction, typing, read, presence)
// to the members who are connected. A client that missed an event refetches.

type hubConn struct {
	ws   *websocket.Conn
	send chan []byte
}

var (
	hubMu    sync.RWMutex
	hubConns = map[int64]map[*hubConn]struct{}{}
)

func presenceOnline(uid int64) bool {
	hubMu.RLock()
	defer hubMu.RUnlock()
	return len(hubConns[uid]) > 0
}

func hubPushUser(uid int64, ev map[string]interface{}) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	hubMu.RLock()
	defer hubMu.RUnlock()
	for c := range hubConns[uid] {
		select {
		case c.send <- raw:
		default: // a stalled client drops events; it refetches on reconnect
		}
	}
}

func hubPushConv(ctx context.Context, conv int64, except int64, ev map[string]interface{}) {
	for _, id := range convMemberIDs(ctx, conv) {
		if id != except {
			hubPushUser(id, ev)
		}
	}
}

func handleMessagesSocket(w http.ResponseWriter, r *http.Request) {
	if msgDB == nil {
		jsonError(w, http.StatusServiceUnavailable, "messages_disabled", "Messaging is not configured.")
		return
	}
	token := parseSocketToken(r)
	user, _, ok := currentUserByToken(r.Context(), token)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "Sign in to use Messages.")
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(4 << 10)
	c := &hubConn{ws: ws, send: make(chan []byte, 64)}
	hubMu.Lock()
	first := len(hubConns[user.ID]) == 0
	if hubConns[user.ID] == nil {
		hubConns[user.ID] = map[*hubConn]struct{}{}
	}
	hubConns[user.ID][c] = struct{}{}
	hubMu.Unlock()
	if first {
		announcePresence(user.ID, true)
	}
	// Everything that was waiting for this person is now on their device:
	// the moment they connect, what they missed is delivered.
	deliverAll(r.Context(), user.ID)

	done := make(chan struct{})
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case raw := <-c.send:
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if ws.WriteMessage(websocket.TextMessage, raw) != nil {
					ws.Close()
					return
				}
			case <-t.C:
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if ws.WriteMessage(websocket.PingMessage, nil) != nil {
					ws.Close()
					return
				}
			case <-done:
				return
			}
		}
	}()

	_ = ws.SetReadDeadline(time.Now().Add(70 * time.Second))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(70 * time.Second))
	})
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			break
		}
		_ = ws.SetReadDeadline(time.Now().Add(70 * time.Second))
		var in struct {
			Type   string `json:"type"`
			Conv   int64  `json:"conv"`
			Typing bool   `json:"typing"`
		}
		if json.Unmarshal(data, &in) != nil || in.Type != "typing" || in.Conv == 0 {
			continue
		}
		if role, acc := memberRole(context.Background(), in.Conv, user.ID); role != "" && acc {
			if in.Typing {
				_, _ = msgDB.Exec(
					`INSERT INTO chat_typing (conv_id,user_id,last_updated) VALUES ($1,$2,now())
					 ON CONFLICT (conv_id,user_id) DO UPDATE SET last_updated=now()`, in.Conv, user.ID)
			} else {
				_, _ = msgDB.Exec(`DELETE FROM chat_typing WHERE conv_id=$1 AND user_id=$2`, in.Conv, user.ID)
			}
			// Somebody who has turned their activity status off is not
			// announced as typing.
			if !activityOn(context.Background(), user.ID) {
				continue
			}
			hubPushConv(context.Background(), in.Conv, user.ID,
				map[string]interface{}{"type": "typing", "conv": in.Conv, "user": user.ID, "typing": in.Typing})
		}
	}
	close(done)
	hubMu.Lock()
	delete(hubConns[user.ID], c)
	last := len(hubConns[user.ID]) == 0
	if last {
		delete(hubConns, user.ID)
	}
	hubMu.Unlock()
	ws.Close()
	if last {
		announcePresence(user.ID, false)
	}
}

// announcePresence tells friends who are connected. A user who hides their
// online status is never announced.
// deliverAll marks everything waiting for this person as delivered and
// pushes the messages they missed.
//
// It runs when their socket opens, so a message sent while they were away
// reaches them the moment they are back rather than sitting unread for
// ever. The message is pushed as that reader sees it, so the reply
// preview and the state are the reader's own.
func deliverAll(ctx context.Context, uid int64) {
	rows, err := msgDB.QueryContext(ctx,
		`SELECT DISTINCT conv_id FROM conversation_members WHERE user_id=$1`, uid)
	if err != nil {
		return
	}
	var convs []int64
	for rows.Next() {
		var c int64
		if rows.Scan(&c) == nil {
			convs = append(convs, c)
		}
	}
	rows.Close()
	for _, c := range convs {
		deliverConversation(ctx, c, uid)
		msgs, err := msgDB.QueryContext(ctx,
			`SELECT id FROM chat_messages WHERE conv_id=$1 AND sender_id<>$2 AND deleted_at IS NULL
			 AND NOT EXISTS (SELECT 1 FROM chat_message_hides h WHERE h.message_id=chat_messages.id AND h.user_id=$3)
			 ORDER BY id DESC LIMIT 40`, c, uid, uid)
		if err != nil {
			continue
		}
		var ids []int64
		for msgs.Next() {
			var id int64
			if msgs.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		msgs.Close()
		for i := len(ids) - 1; i >= 0; i-- {
			m := loadMessageFor(ids[i], uid)
			if m != nil {
				hubPushUser(uid, map[string]interface{}{"type": "message", "message": m})
			}
		}
	}
}

// announcePresence tells friends who is connected. A user who hides their
// online status is never announced.
func announcePresence(uid int64, online bool) {
	var show bool = true
	_ = msgDB.QueryRow(`SELECT show_online FROM social_profiles WHERE user_id=$1`, uid).Scan(&show)
	if !show {
		return
	}
	// The activity switch covers online state as well as typing: somebody
	// who turns it off is not announced at all.
	if !activityOn(context.Background(), uid) {
		return
	}
	rows, err := msgDB.Query(
		`SELECT CASE WHEN user_lo=$1 THEN user_hi ELSE user_lo END FROM friendships
		 WHERE (user_lo=$1 OR user_hi=$1) AND status='accepted'`, uid)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var f int64
		if rows.Scan(&f) == nil {
			hubPushUser(f, map[string]interface{}{"type": "presence", "user": uid, "online": online})
		}
	}
}

func currentUserByToken(ctx context.Context, token string) (AccountUser, AccountSession, bool) {
	if token == "" {
		return AccountUser{}, AccountSession{}, false
	}
	session, ok, err := accountStore.Session(ctx, hashToken(token))
	if err != nil || !ok || !session.ExpiresAt.After(time.Now()) {
		return AccountUser{}, AccountSession{}, false
	}
	user, ok, err := accountStore.UserByID(ctx, session.UserID)
	if err != nil || !ok {
		return AccountUser{}, AccountSession{}, false
	}
	return user, session, true
}
