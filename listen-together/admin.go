package main

// The admin panel (2026-11-08): one page that answers "kaun hai, kitne hain,
// kya kar rahe hain" without a database client.
//
// It is guarded by JAM_ADMIN_KEY in the query string, the same key
// GET /api/admin/stats takes, and an empty key disables it entirely — a
// deployment that never set one has no admin surface to forget about.
//
// The page deliberately shows no credential and no document: a playlist count
// and a byte size are enough to spot an account that is using sync, and a
// stolen admin link then cannot be turned into a stolen Discord token.

import (
	"crypto/subtle"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/KabirSinghBhatia/BitChord/backend/config"
)

// adminUserRow is one line of the panel's table: the account, and what its
// sync document holds.
type adminUserRow struct {
	ID         int64
	Name       string
	Email      string
	Google     bool
	Created    time.Time
	LastLogin  time.Time
	Logins     int64
	Playlists  int
	SyncBytes  int
	SyncAt     time.Time
	HasSync    bool
	LastSeenIn string
}

type adminPageData struct {
	Now        time.Time
	Stats      AccountStats
	Users      []adminUserRow
	SyncUsers  int
	SyncBytes  int
	SyncPlays  int
	ServerUp   string
	AccountsUp bool
}

var adminTemplate = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Bitify admin</title>
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body {
  margin: 0; padding: 28px 20px 60px;
  background: #0b0b0d; color: #e8e8ea;
  font: 15px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
}
h1 { font-size: 22px; margin: 0 0 4px; letter-spacing: -0.02em; }
.sub { color: #8a8a93; font-size: 13px; margin-bottom: 24px; }
.cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; margin-bottom: 28px; }
.card { background: #141418; border: 1px solid #232329; border-radius: 14px; padding: 14px 16px; }
.card .n { font-size: 26px; font-weight: 650; letter-spacing: -0.03em; }
.card .l { color: #8a8a93; font-size: 12px; text-transform: uppercase; letter-spacing: 0.06em; margin-top: 2px; }
.wrap { overflow-x: auto; border: 1px solid #232329; border-radius: 14px; }
table { width: 100%; border-collapse: collapse; font-size: 13.5px; }
th, td { text-align: left; padding: 10px 14px; white-space: nowrap; }
thead th { background: #141418; color: #8a8a93; font-weight: 600; font-size: 11.5px; text-transform: uppercase; letter-spacing: 0.06em; position: sticky; top: 0; }
tbody tr + tr td { border-top: 1px solid #1c1c22; }
tbody tr:hover td { background: #121216; }
.email { color: #9aa0ff; }
.muted { color: #6f6f78; }
.pill { display: inline-block; padding: 1px 8px; border-radius: 999px; font-size: 11.5px; background: #1d2a1f; color: #7ee2a8; }
.empty { padding: 26px 16px; color: #8a8a93; }
</style>
</head>
<body>
<h1>Bitify admin</h1>
<div class="sub">{{.ServerUp}} · {{.Now.Format "02 Jan 2006, 15:04 MST"}}</div>

<div class="cards">
  <div class="card"><div class="n">{{.Stats.TotalUsers}}</div><div class="l">Users</div></div>
  <div class="card"><div class="n">{{.Stats.GoogleUsers}}</div><div class="l">Google</div></div>
  <div class="card"><div class="n">{{.Stats.ActiveToday}}</div><div class="l">Active today</div></div>
  <div class="card"><div class="n">{{.Stats.Active7d}}</div><div class="l">Active 7d</div></div>
  <div class="card"><div class="n">{{.Stats.ActiveSessions}}</div><div class="l">Live sessions</div></div>
  <div class="card"><div class="n">{{.Stats.TotalLogins}}</div><div class="l">Logins</div></div>
  <div class="card"><div class="n">{{.SyncUsers}}</div><div class="l">Syncing</div></div>
  <div class="card"><div class="n">{{.SyncPlays}}</div><div class="l">Playlists</div></div>
</div>

{{if .AccountsUp}}
<div class="wrap">
<table>
<thead><tr>
  <th>#</th><th>Name</th><th>Email</th><th>Signed up</th><th>Last login</th>
  <th>Logins</th><th>Playlists</th><th>Sync size</th><th>Synced</th>
</tr></thead>
<tbody>
{{range .Users}}
<tr>
  <td class="muted">{{.ID}}</td>
  <td>{{.Name}}</td>
  <td class="email">{{.Email}} {{if .Google}}<span class="pill">Google</span>{{end}}</td>
  <td class="muted">{{.Created.Format "02 Jan 06"}}</td>
  <td class="muted">{{.LastSeenIn}}</td>
  <td>{{.Logins}}</td>
  <td>{{if .HasSync}}{{.Playlists}}{{else}}<span class="muted">—</span>{{end}}</td>
  <td>{{if .HasSync}}{{.SyncBytes}} B{{else}}<span class="muted">—</span>{{end}}</td>
  <td class="muted">{{if .HasSync}}{{.SyncAt.Format "02 Jan 15:04"}}{{else}}—{{end}}</td>
</tr>
{{else}}
<tr><td colspan="9" class="empty">No accounts yet.</td></tr>
{{end}}
</tbody>
</table>
</div>
{{else}}
<div class="wrap"><div class="empty">Accounts are not configured on this server (no DATABASE_URL).</div></div>
{{end}}
</body>
</html>`))

func handleAdminPanel(w http.ResponseWriter, r *http.Request) {
	if config.AdminKey == "" {
		http.NotFound(w, r)
		return
	}
	// Constant time, so the panel's key cannot be guessed a byte at a time.
	given := r.URL.Query().Get("key")
	if subtle.ConstantTimeCompare([]byte(given), []byte(config.AdminKey)) != 1 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Not found. Add ?key=<JAM_ADMIN_KEY>.\n"))
		return
	}

	now := time.Now()
	data := adminPageData{
		Now:        now,
		ServerUp:   "Bitify server",
		AccountsUp: accountStore != nil,
	}
	if accountStore == nil {
		_ = adminTemplate.Execute(w, data)
		return
	}

	stats, err := accountStore.Stats(r.Context(), now)
	if err != nil {
		log.Printf("admin: stats failed: %v", err)
	}
	data.Stats = stats

	users, err := accountStore.ListUsers(r.Context(), 500)
	if err != nil {
		log.Printf("admin: list failed: %v", err)
	}
	summaries, err := accountStore.SyncSummaries(r.Context())
	if err != nil {
		log.Printf("admin: sync list failed: %v", err)
	}
	byUser := make(map[int64]SyncSummary, len(summaries))
	for _, summary := range summaries {
		byUser[summary.UserID] = summary
		data.SyncUsers++
		data.SyncBytes += summary.Bytes
		data.SyncPlays += summary.PlaylistCount
	}

	for _, user := range users {
		row := adminUserRow{
			ID:        user.ID,
			Name:      user.Name,
			Email:     user.Email,
			Google:    user.GoogleSub != "",
			Created:   user.CreatedAt,
			LastLogin: user.LastLoginAt,
			Logins:    user.LoginCount,
			LastSeenIn: "never",
		}
		if !user.LastLoginAt.IsZero() {
			row.LastSeenIn = humanAgo(now.Sub(user.LastLoginAt))
		}
		if summary, ok := byUser[user.ID]; ok {
			row.HasSync = true
			row.Playlists = summary.PlaylistCount
			row.SyncBytes = summary.Bytes
			row.SyncAt = summary.UpdatedAt
		}
		data.Users = append(data.Users, row)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := adminTemplate.Execute(w, data); err != nil {
		log.Printf("admin: render failed: %v", err)
	}
}

// humanAgo turns a duration into the shortest phrase that still says when.
// The panel is read at a glance, so "3d ago" beats a timestamp.
func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	case d < 30*24*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	default:
		return strconv.Itoa(int(d.Hours()/24/30)) + "mo ago"
	}
}
