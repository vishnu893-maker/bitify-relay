package main

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

// profileView never carries an email: only what a stranger may see.
func loadProfileView(uid int64, forViewer int64) (map[string]interface{}, bool) {
	var username, display, bio, avatar, vis string
	var online bool
	err := msgDB.QueryRow(
		`SELECT p.username, p.display_name, p.bio, p.avatar_url, p.activity_visibility, p.show_online
		 FROM social_profiles p WHERE p.user_id=$1`, uid).Scan(&username, &display, &bio, &avatar, &vis, &online)
	if err == sql.ErrNoRows {
		var name string
		if msgDB.QueryRow(`SELECT name FROM app_users WHERE id=$1`, uid).Scan(&name) != nil {
			return nil, false
		}
		display = name
	} else if err != nil {
		return nil, false
	}
	rel := "none"
	if forViewer == uid {
		rel = "self"
	} else {
		lo, hi := pair(forViewer, uid)
		var st string
		var by int64
		if msgDB.QueryRow(`SELECT status, requested_by FROM friendships WHERE user_lo=$1 AND user_hi=$2`, lo, hi).Scan(&st, &by) == nil {
			if st == "accepted" {
				rel = "friend"
			} else if by == forViewer {
				rel = "outgoing"
			} else {
				rel = "incoming"
			}
		}
	}
	return map[string]interface{}{
		"id": uid, "username": username, "displayName": display, "bio": bio,
		"avatarUrl": avatar, "relation": rel, "online": online && presenceOnline(uid),
	}, true
}

func handleSocialMe(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPut {
		var in struct {
			Username    string `json:"username"`
			DisplayName string `json:"displayName"`
			Bio         string `json:"bio"`
			AvatarURL   string `json:"avatarUrl"`
		}
		if !readBody(w, r, &in) || !msgAllow(w, user.ID) {
			return
		}
		un := strings.TrimSpace(in.Username)
		if un != "" && !validUsername(un) {
			jsonError(w, http.StatusBadRequest, "bad_username", "Usernames are 3-24 letters, digits, . or _.")
			return
		}
		// The privacy and notification switches are not touched here: they
		// have their own endpoints, so saving a name cannot quietly reset
		// them.
		_, err := msgDB.Exec(
			`INSERT INTO social_profiles (user_id, username, display_name, bio, avatar_url)
			 VALUES ($1,$2,$3,$4,$5)
			 ON CONFLICT (user_id) DO UPDATE SET username=$2, display_name=$3, bio=$4, avatar_url=$5`,
			user.ID, un, clip(in.DisplayName, 40), clip(in.Bio, 160), clip(in.AvatarURL, 300))
		if err != nil {
			jsonError(w, http.StatusConflict, "username_taken", "That username is taken.")
			return
		}
	}
	view, _ := loadProfileView(user.ID, user.ID)
	var vis string
	var show bool
	var notifyMessages, notifyActivity bool
	_ = msgDB.QueryRow(
		`SELECT activity_visibility, show_online, notify_messages, notify_activity
		 FROM social_profiles WHERE user_id=$1`, user.ID).Scan(&vis, &show, &notifyMessages, &notifyActivity)
	if vis == "" {
		// No profile row yet: the defaults are the ones the schema declares,
		// and the switches start on. Reporting them as off here would show a
		// new listener two switches that are off, when the server has never
		// been asked to turn them off.
		vis, show = "friends", true
		notifyMessages, notifyActivity = true, true
	}
	view["activityVisibility"] = vis
	view["showOnline"] = show
	view["notifyMessages"] = notifyMessages
	view["notifyActivity"] = notifyActivity
	jsonResponse(w, http.StatusOK, map[string]interface{}{"profile": view})
}

// handleSocialPrivacy saves who sees the listener's music activity and whether
// they look online, on their own so the rest of the profile is untouched.
func handleSocialPrivacy(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPut {
		var in struct {
			ActivityVisibility string `json:"activityVisibility"`
			ShowOnline         *bool  `json:"showOnline"`
		}
		if !readBody(w, r, &in) || !msgAllow(w, user.ID) {
			return
		}
		vis := in.ActivityVisibility
		if vis != "everyone" && vis != "friends" && vis != "nobody" {
			vis = "friends"
		}
		// A listener with no profile row yet is online by default; reading
		// "off" out of the missing row would write it back that way.
		show := true
		_ = msgDB.QueryRow(`SELECT show_online FROM social_profiles WHERE user_id=$1`, user.ID).Scan(&show)
		if in.ShowOnline != nil {
			show = *in.ShowOnline
		}
		_, _ = msgDB.Exec(
			`INSERT INTO social_profiles (user_id, activity_visibility, show_online)
			 VALUES ($1,$2,$3)
			 ON CONFLICT (user_id) DO UPDATE SET activity_visibility=$2, show_online=$3`,
			user.ID, vis, show)
	}
	var vis string
	var show bool
	_ = msgDB.QueryRow(`SELECT activity_visibility, show_online FROM social_profiles WHERE user_id=$1`, user.ID).Scan(&vis, &show)
	if vis == "" {
		vis, show = "friends", true
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"activityVisibility": vis, "showOnline": show,
	})
}

// handleSocialNotifications saves the two notification switches on their own, so
// a client that only wants to turn one of them off does not have to send the
// rest of the profile with it — and cannot lose the username by accident.
func handleSocialNotifications(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPut {
		var in struct {
			NotifyMessages *bool `json:"notifyMessages"`
			NotifyActivity *bool `json:"notifyActivity"`
		}
		if !readBody(w, r, &in) || !msgAllow(w, user.ID) {
			return
		}
		// The switches start on. A listener who has no profile row yet has
		// never turned them off, so reading "off" out of the missing row and
		// writing it back would silently switch off the one they did not
		// touch.
		notifyMessages, notifyActivity := true, true
		_ = msgDB.QueryRow(
			`SELECT notify_messages, notify_activity FROM social_profiles WHERE user_id=$1`, user.ID).
			Scan(&notifyMessages, &notifyActivity)
		if in.NotifyMessages != nil {
			notifyMessages = *in.NotifyMessages
		}
		if in.NotifyActivity != nil {
			notifyActivity = *in.NotifyActivity
		}
		_, _ = msgDB.Exec(
			`INSERT INTO social_profiles (user_id, notify_messages, notify_activity)
			 VALUES ($1,$2,$3)
			 ON CONFLICT (user_id) DO UPDATE SET notify_messages=$2, notify_activity=$3`,
			user.ID, notifyMessages, notifyActivity)
	}
	notifyMessages, notifyActivity := true, true
	_ = msgDB.QueryRow(
		`SELECT notify_messages, notify_activity FROM social_profiles WHERE user_id=$1`, user.ID).
		Scan(&notifyMessages, &notifyActivity)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"notifyMessages": notifyMessages, "notifyActivity": notifyActivity,
	})
}

func handleUserSearch(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len(q) < 2 || !msgAllow(w, user.ID) {
		jsonResponse(w, http.StatusOK, map[string]interface{}{"users": []interface{}{}})
		return
	}
	q = strings.NewReplacer(`\`, ``, `%`, ``, `_`, `\_`).Replace(q)
	rows, err := msgDB.Query(
		`SELECT user_id FROM social_profiles WHERE lower(username) LIKE $1 AND username <> ''
		 AND user_id <> $2
		 AND NOT EXISTS (SELECT 1 FROM social_blocks WHERE (blocker=$2 AND blocked=user_id) OR (blocker=user_id AND blocked=$2))
		 ORDER BY username LIMIT 20`, q+"%", user.ID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Search failed.")
		return
	}
	defer rows.Close()
	out := []interface{}{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			if v, ok := loadProfileView(id, user.ID); ok {
				out = append(out, v)
			}
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"users": out})
}

func handleUserGet(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if isBlockedEitherWay(r.Context(), user.ID, id) {
		jsonError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	}
	v, found := loadProfileView(id, user.ID)
	if !found {
		jsonError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"profile": v})
}

func handleFriendsList(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok {
		return
	}
	rows, err := msgDB.Query(
		`SELECT CASE WHEN user_lo=$1 THEN user_hi ELSE user_lo END FROM friendships
		 WHERE (user_lo=$1 OR user_hi=$1) AND status IN ('accepted','pending')`, user.ID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db", "Could not load friends.")
		return
	}
	defer rows.Close()
	out := []interface{}{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			if v, ok := loadProfileView(id, user.ID); ok {
				out = append(out, v)
			}
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"friends": out})
}

// handleFriendAction: request | accept | reject | remove | block | unblock.
func handleFriendAction(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	other, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	action := r.PathValue("action")
	if other == 0 || other == user.ID {
		jsonError(w, http.StatusBadRequest, "bad_request", "Invalid user.")
		return
	}
	var exists int
	if msgDB.QueryRow(`SELECT 1 FROM app_users WHERE id=$1`, other).Scan(&exists) != nil {
		jsonError(w, http.StatusNotFound, "not_found", "No such user.")
		return
	}
	lo, hi := pair(user.ID, other)
	switch action {
	case "request":
		if isBlockedEitherWay(r.Context(), user.ID, other) {
			jsonError(w, http.StatusForbidden, "blocked", "You can't send this request.")
			return
		}
		res, _ := msgDB.Exec(
			`INSERT INTO friendships (user_lo,user_hi,status,requested_by) VALUES ($1,$2,'pending',$3)
			 ON CONFLICT DO NOTHING`, lo, hi, user.ID)
		if n, _ := res.RowsAffected(); n == 1 {
			hubPushUser(other, map[string]interface{}{"type": "friend_request", "from": user.ID})
		}
	case "accept":
		res, _ := msgDB.Exec(
			`UPDATE friendships SET status='accepted' WHERE user_lo=$1 AND user_hi=$2 AND status='pending' AND requested_by<>$3`,
			lo, hi, user.ID)
		if n, _ := res.RowsAffected(); n == 1 {
			hubPushUser(other, map[string]interface{}{"type": "friend_accepted", "from": user.ID})
		}
	case "reject", "remove":
		_, _ = msgDB.Exec(`DELETE FROM friendships WHERE user_lo=$1 AND user_hi=$2`, lo, hi)
	case "block":
		_, _ = msgDB.Exec(`DELETE FROM friendships WHERE user_lo=$1 AND user_hi=$2`, lo, hi)
		_, _ = msgDB.Exec(`INSERT INTO social_blocks (blocker, blocked) VALUES ($1,$2) ON CONFLICT DO NOTHING`, user.ID, other)
	case "unblock":
		_, _ = msgDB.Exec(`DELETE FROM social_blocks WHERE blocker=$1 AND blocked=$2`, user.ID, other)
	default:
		jsonError(w, http.StatusBadRequest, "bad_request", "Unknown action.")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func handleReport(w http.ResponseWriter, r *http.Request) {
	user, ok := msgAuth(w, r)
	if !ok || !msgAllow(w, user.ID) {
		return
	}
	var in struct {
		UserID    int64  `json:"userId"`
		MessageID int64  `json:"messageId"`
		Reason    string `json:"reason"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if in.UserID == 0 || strings.TrimSpace(in.Reason) == "" {
		jsonError(w, http.StatusBadRequest, "bad_request", "Say who and why.")
		return
	}
	_, _ = msgDB.Exec(`INSERT INTO social_reports (reporter, target_user, message_id, reason) VALUES ($1,$2,$3,$4)`,
		user.ID, in.UserID, in.MessageID, clip(in.Reason, 500))
	jsonResponse(w, http.StatusOK, map[string]interface{}{"ok": true})
}
