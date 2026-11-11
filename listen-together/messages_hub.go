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
			Type string `json:"type"`
			Conv int64  `json:"conv"`
		}
		if json.Unmarshal(data, &in) != nil || in.Type != "typing" || in.Conv == 0 {
			continue
		}
		if role, acc := memberRole(context.Background(), in.Conv, user.ID); role != "" && acc {
			hubPushConv(context.Background(), in.Conv, user.ID,
				map[string]interface{}{"type": "typing", "conv": in.Conv, "user": user.ID})
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
func announcePresence(uid int64, online bool) {
	var show bool = true
	_ = msgDB.QueryRow(`SELECT show_online FROM social_profiles WHERE user_id=$1`, uid).Scan(&show)
	if !show {
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
