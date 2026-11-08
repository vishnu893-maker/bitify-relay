package main

// The listening vault (2026-10-24, additive since 2026-10-31): play minutes
// that survive a reinstall and add up across every device the account uses.
//
// A signed-in device (web, phone, desktop) keeps its Replay stats locally, so
// downloading the app again wipes every minute of it. This vault ties the
// stats to the account instead: the client uploads its month buckets keyed by
// a hash of its login — never the token — under a per-install device id, and
// pulls the account's merged months back on a fresh install.
//
// The first version of this file kept one merged copy per account and merged
// a phone's and a PC's month with max-per-day, so two devices listening on the
// same evening counted the busier one and never their sum. Now every device
// keeps its own buckets and the read is a sum. Months written before the
// change are kept as a baseline: days before the account's cutover date are
// still deduped with max, so an old max-merged copy cannot be counted twice;
// every day on or after it is the true sum of what each device heard.
//
// Storage is a single JSON file beside the server. It survives Render's
// sleep; a redeploy starts it empty, and the next signed-in client
// repopulates it from its own local copy. Buckets are capped so one client
// cannot grow this into a database.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	vaultMaxUserBytes = 768 * 1024
	vaultMaxUsers     = 4000
	vaultMaxMonths    = 48
	vaultMaxDevices   = 8
)

type vaultDayStats struct {
	Ms    int64 `json:"ms"`
	Plays int   `json:"plays"`
}

type vaultTrack struct {
	ID       string                   `json:"id"`
	Title    string                   `json:"title,omitempty"`
	Artist   string                   `json:"artist,omitempty"`
	Album    *string                  `json:"album,omitempty"`
	AlbumID  *string                  `json:"albumId,omitempty"`
	ArtistID *string                  `json:"artistId,omitempty"`
	Art      *string                  `json:"art,omitempty"`
	Ms       int64                    `json:"ms"`
	Plays    int                      `json:"plays"`
	Last     int64                    `json:"last,omitempty"`
	Days     map[string]vaultDayStats `json:"days,omitempty"`
}

type vaultName struct {
	Name  string                   `json:"name"`
	Sub   *string                  `json:"sub,omitempty"`
	Art   *string                  `json:"art,omitempty"`
	ID    *string                  `json:"id,omitempty"`
	Ms    int64                    `json:"ms"`
	Plays int                      `json:"plays"`
	Key   *string                  `json:"key,omitempty"`
	Days  map[string]vaultDayStats `json:"days,omitempty"`
}

type vaultBucket struct {
	Version    int                      `json:"version"`
	Month      string                   `json:"month"`
	Tracks     []vaultTrack             `json:"tracks"`
	Artists    []vaultName              `json:"artists"`
	Albums     []vaultName              `json:"albums"`
	Hours      []int64                  `json:"hours"`
	Days       map[string]int64         `json:"days"`
	DaysISO    map[string]int64         `json:"daysIso,omitempty"`
	TrackDays  map[string]vaultDayStats `json:"trackDays,omitempty"`
	ArtistDays map[string]vaultDayStats `json:"artistDays,omitempty"`
	AlbumDays  map[string]vaultDayStats `json:"albumDays,omitempty"`
}

// vaultAccount is one login's vault. Devices are kept apart so the read can
// add them together; Cutover is the first day whose minutes are additive.
type vaultAccount struct {
	UpdatedAt int64                    `json:"updatedAt"`
	Cutover   string                   `json:"cutover,omitempty"`
	Devices   map[string][]vaultBucket `json:"devices"`
}

// vaultEntry is the pre-2026-10-31 shape, read only to migrate an existing
// file. A new write never produces it.
type vaultEntry struct {
	UpdatedAt int64           `json:"updatedAt"`
	Buckets   json.RawMessage `json:"buckets"`
}

var (
	vaultMu   sync.Mutex
	vaultData map[string]vaultAccount
	vaultFile string
)

func vaultLoad() {
	vaultFile = os.Getenv("VAULT_PATH")
	if vaultFile == "" {
		vaultFile = "vault.json"
	}
	vaultData = make(map[string]vaultAccount)
	raw, err := os.ReadFile(vaultFile)
	if err != nil {
		return
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil || probe == nil {
		return
	}
	today := time.Now().Format("2006-01-02")
	for id, body := range probe {
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			continue
		}
		if _, ok := fields["devices"]; ok {
			var account vaultAccount
			if json.Unmarshal(body, &account) != nil {
				continue
			}
			if account.Devices == nil {
				account.Devices = map[string][]vaultBucket{}
			}
			vaultData[id] = account
			continue
		}
		// The old shape: one merged array under "buckets". Keep it as a
		// baseline device and start additive counting from today, so the
		// max-merged copy is never added to a device's own copy.
		if _, ok := fields["buckets"]; !ok {
			continue
		}
		var entry vaultEntry
		if json.Unmarshal(body, &entry) != nil {
			continue
		}
		var buckets []vaultBucket
		if json.Unmarshal(entry.Buckets, &buckets) != nil || len(buckets) == 0 {
			continue
		}
		vaultData[id] = vaultAccount{
			UpdatedAt: entry.UpdatedAt,
			Cutover:   today,
			Devices:   map[string][]vaultBucket{"legacy": buckets},
		}
	}
}

func vaultSaveLocked() {
	raw, err := json.Marshal(vaultData)
	if err != nil {
		return
	}
	tmp := vaultFile + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		os.Rename(tmp, vaultFile)
	}
}

// handleVaultGet answers with the account's merged months, 404 when there are
// none — a fresh install pulls an empty answer and simply keeps its own.
func handleVaultGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("userId")
	if !validVaultId(id) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_user_id"})
		return
	}
	vaultMu.Lock()
	account, ok := vaultData[id]
	if !ok || len(account.Devices) == 0 {
		vaultMu.Unlock()
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": "empty_vault"})
		return
	}
	merged := vaultMerge(account)
	// A client that names itself gets its own last upload back beside the
	// merged account. That is how it knows which of its local minutes the
	// server already has, so it can add only the unsynced rest instead of
	// counting the same evening twice. An old client sends no device and
	// still receives the bare array it has always parsed.
	var own []vaultBucket
	deviceID := r.URL.Query().Get("device")
	if validDeviceId(deviceID) {
		own = account.Devices[deviceID]
	}
	vaultMu.Unlock()
	if len(merged) == 0 {
		jsonResponse(w, http.StatusNotFound, map[string]interface{}{"error": "empty_vault"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if validDeviceId(deviceID) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"merged": merged,
			"device": own,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(merged)
}

type vaultPush struct {
	DeviceID string        `json:"deviceId"`
	Buckets  []vaultBucket `json:"buckets"`
}

// handleVaultPut stores one device's months. The body is either the new
// {"deviceId": "...", "buckets": [...]} wrapper or the old bare array; an old
// client's array is kept under the "legacy" device so it still reads back.
func handleVaultPut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("userId")
	if !validVaultId(id) {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_user_id"})
		return
	}
	// Read to the end through a LimitReader, not one Read call: Read may hand
	// back a partial body, and behind Render's proxy a vault of any real size
	// arrives in several chunks. The single Read truncated the JSON and had
	// every large vault rejected as bad_buckets.
	body, err := io.ReadAll(io.LimitReader(r.Body, vaultMaxUserBytes+1))
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_body"})
		return
	}
	if len(body) > vaultMaxUserBytes {
		jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]interface{}{"error": "vault_too_large"})
		return
	}

	deviceID := "legacy"
	var buckets []vaultBucket
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		var push vaultPush
		if json.Unmarshal(body, &push) != nil || !validDeviceId(push.DeviceID) {
			jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_device_id"})
			return
		}
		deviceID = push.DeviceID
		buckets = push.Buckets
	} else if json.Unmarshal(body, &buckets) != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_buckets"})
		return
	}
	if len(buckets) > vaultMaxMonths {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_buckets"})
		return
	}
	for i := range buckets {
		if !validMonth(buckets[i].Month) {
			jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"error": "bad_buckets"})
			return
		}
		if buckets[i].Hours == nil {
			buckets[i].Hours = make([]int64, 24)
		}
	}

	vaultMu.Lock()
	account, exists := vaultData[id]
	if !exists {
		if len(vaultData) >= vaultMaxUsers {
			vaultMu.Unlock()
			jsonResponse(w, http.StatusInsufficientStorage, map[string]interface{}{"error": "vault_full"})
			return
		}
		account = vaultAccount{
			Devices: make(map[string][]vaultBucket),
			// The first day whose minutes add across devices. A device's
			// history from before this build is already merged into the
			// others on a second install, so only the days from here on can
			// be summed without counting the same evening twice.
			Cutover: time.Now().Format("2006-01-02"),
		}
	}
	if account.Devices == nil {
		account.Devices = make(map[string][]vaultBucket)
	}
	if _, known := account.Devices[deviceID]; !known && len(account.Devices) >= vaultMaxDevices {
		// Drop one of the old device rows rather than refusing the person who
		// just installed the app on a new machine. The baseline is kept.
		for key := range account.Devices {
			if key == "legacy" || key == deviceID {
				continue
			}
			delete(account.Devices, key)
			break
		}
	}
	account.Devices[deviceID] = buckets
	account.UpdatedAt = time.Now().UnixMilli()
	vaultData[id] = account
	vaultSaveLocked()
	merged := vaultMerge(account)
	deviceCount := len(account.Devices)
	vaultMu.Unlock()

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"bytes":   len(body),
		"devices": deviceCount,
		"merged":  merged,
	})
}

// A vault key is the client-side hash of the login: hex, fixed length.
func validVaultId(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// A device id is a random install token, not an account id: keep it short and
// boring so it can be a JSON key without surprises.
func validDeviceId(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func validMonth(month string) bool {
	if len(month) != 7 || month[4] != '-' {
		return false
	}
	for i, c := range month {
		if i == 4 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	m, err := strconv.Atoi(month[5:])
	return err == nil && m >= 1 && m <= 12
}

// vaultMerge adds every device's months together. Days before the account's
// cutover are deduped with max; days on or after it are summed. That is the
// one rule that makes an old max-merged copy safe to keep beside the new
// per-device copies without double counting the same evening.
func vaultMerge(account vaultAccount) []vaultBucket {
	byMonth := make(map[string][]vaultBucket)
	for _, buckets := range account.Devices {
		for _, bucket := range buckets {
			if !validMonth(bucket.Month) {
				continue
			}
			byMonth[bucket.Month] = append(byMonth[bucket.Month], bucket)
		}
	}
	months := make([]string, 0, len(byMonth))
	for month := range byMonth {
		months = append(months, month)
	}
	sort.Strings(months)
	merged := make([]vaultBucket, 0, len(months))
	for _, month := range months {
		merged = append(merged, mergeMonth(month, byMonth[month], account.Cutover))
	}
	return merged
}

func mergeMonth(month string, buckets []vaultBucket, cutover string) vaultBucket {
	out := vaultBucket{
		Version: 1,
		Month:   month,
		Hours:   make([]int64, 24),
		Days:    map[string]int64{},
	}
	daysISO := map[string]int64{}
	trackDays := map[string]vaultDayStats{}
	artistDays := map[string]vaultDayStats{}
	albumDays := map[string]vaultDayStats{}

	cutoverMonth := ""
	if len(cutover) >= 7 {
		cutoverMonth = cutover[:7]
	}
	// Hours have no date, so a whole cutover month is deduped; later months
	// are a plain sum. The chart is a shape, not a total, so this is the
	// honest approximation the shape allows.
	sumHours := cutover == "" || month > cutoverMonth

	tracks := map[string]*mergedTrack{}
	artists := map[string]*mergedName{}
	albums := map[string]*mergedName{}

	for _, bucket := range buckets {
		for key, ms := range bucket.Days {
			iso, ok := isoDayKey(month, key)
			if !ok {
				continue
			}
			if cutover != "" && iso < cutover {
				daysISO[iso] = maxInt64(daysISO[iso], ms)
			} else {
				daysISO[iso] += ms
			}
		}
		for hour := 0; hour < 24; hour++ {
			ms := int64(0)
			if hour < len(bucket.Hours) {
				ms = bucket.Hours[hour]
			}
			if sumHours {
				out.Hours[hour] += ms
			} else {
				out.Hours[hour] = maxInt64(out.Hours[hour], ms)
			}
		}
		for _, track := range bucket.Tracks {
			if track.ID == "" {
				continue
			}
			entry := tracks[track.ID]
			if entry == nil {
				entry = &mergedTrack{track: track}
				tracks[track.ID] = entry
			}
			entry.add(track, month, cutover)
		}
		for _, artist := range bucket.Artists {
			if artist.Name == "" {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(artist.Name))
			entry := artists[key]
			if entry == nil {
				entry = &mergedName{name: artist}
				artists[key] = entry
			}
			entry.add(artist, month, cutover)
		}
		for _, album := range bucket.Albums {
			if album.Name == "" {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(album.Name)) + "\x00" + strings.ToLower(strings.TrimSpace(deref(album.Sub)))
			entry := albums[key]
			if entry == nil {
				entry = &mergedName{name: album}
				albums[key] = entry
			}
			entry.add(album, month, cutover)
		}
		mergeDayStats(trackDays, bucket.TrackDays, cutover)
		mergeDayStats(artistDays, bucket.ArtistDays, cutover)
		mergeDayStats(albumDays, bucket.AlbumDays, cutover)
	}

	for iso, ms := range daysISO {
		day, ok := dayOfMonth(iso)
		if !ok {
			continue
		}
		out.Days[strconv.Itoa(day)] = ms
	}
	out.DaysISO = daysISO
	if len(trackDays) > 0 {
		out.TrackDays = trackDays
	}
	if len(artistDays) > 0 {
		out.ArtistDays = artistDays
	}
	if len(albumDays) > 0 {
		out.AlbumDays = albumDays
	}

	out.Tracks = make([]vaultTrack, 0, len(tracks))
	for _, entry := range tracks {
		ms, plays := entry.total()
		if ms <= 0 && plays <= 0 {
			continue
		}
		track := entry.track
		track.Ms = ms
		track.Plays = plays
		if len(entry.days) > 0 {
			track.Days = entry.days
		}
		out.Tracks = append(out.Tracks, track)
	}
	sort.Slice(out.Tracks, func(i, j int) bool { return out.Tracks[i].Ms > out.Tracks[j].Ms })

	out.Artists = make([]vaultName, 0, len(artists))
	for _, entry := range artists {
		ms, plays := entry.total()
		if ms <= 0 && plays <= 0 {
			continue
		}
		name := entry.name
		name.Ms = ms
		name.Plays = plays
		if len(entry.days) > 0 {
			name.Days = entry.days
		}
		out.Artists = append(out.Artists, name)
	}
	sort.Slice(out.Artists, func(i, j int) bool { return out.Artists[i].Ms > out.Artists[j].Ms })

	out.Albums = make([]vaultName, 0, len(albums))
	for _, entry := range albums {
		ms, plays := entry.total()
		if ms <= 0 && plays <= 0 {
			continue
		}
		name := entry.name
		name.Ms = ms
		name.Plays = plays
		if len(entry.days) > 0 {
			name.Days = entry.days
		}
		out.Albums = append(out.Albums, name)
	}
	sort.Slice(out.Albums, func(i, j int) bool { return out.Albums[i].Ms > out.Albums[j].Ms })
	return out
}

// mergedTrack keeps the metadata of the busiest copy and adds the minutes
// either side of the cutover with the matching rule.
type mergedTrack struct {
	track     vaultTrack
	preSeen   bool
	preMs     int64
	prePlays  int
	postMs    int64
	postPlays int
	days      map[string]vaultDayStats
}

func (m *mergedTrack) add(track vaultTrack, month string, cutover string) {
	preMs, prePlays, postMs, postPlays := splitTrack(track, month, cutover)
	if cutover == "" {
		m.postMs += track.Ms
		m.postPlays += track.Plays
	} else {
		if !m.preSeen {
			m.preMs, m.prePlays, m.preSeen = preMs, prePlays, true
		} else {
			m.preMs = maxInt64(m.preMs, preMs)
			m.prePlays = maxInt(m.prePlays, prePlays)
		}
		m.postMs += postMs
		m.postPlays += postPlays
	}
	if len(track.Days) > 0 {
		if m.days == nil {
			m.days = map[string]vaultDayStats{}
		}
		mergeDayStats(m.days, track.Days, cutover)
	}
	if track.Ms >= m.track.Ms {
		m.track = track
	}
}

func (m *mergedTrack) total() (int64, int) {
	return m.preMs + m.postMs, m.prePlays + m.postPlays
}

// The pre/post halves of a track. A track with a per-day map can be split
// exactly; one without it (an older desktop bucket) is filed by its month.
func splitTrack(track vaultTrack, month string, cutover string) (preMs int64, prePlays int, postMs int64, postPlays int) {
	if cutover == "" {
		return 0, 0, track.Ms, track.Plays
	}
	if len(track.Days) > 0 {
		var summedMs int64
		var summedPlays int
		for day, stats := range track.Days {
			summedMs += stats.Ms
			summedPlays += stats.Plays
			if day < cutover {
				preMs += stats.Ms
				prePlays += stats.Plays
			} else {
				postMs += stats.Ms
				postPlays += stats.Plays
			}
		}
		// A partial day map (an old client) still has to account for the
		// rest of the track's total; treat the remainder as current.
		if remainder := track.Ms - summedMs; remainder > 0 {
			postMs += remainder
		}
		if remainder := track.Plays - summedPlays; remainder > 0 {
			postPlays += remainder
		}
		return preMs, prePlays, postMs, postPlays
	}
	if month <= cutover[:7] {
		// No day map: this month's copy may already hold the other device's
		// old minutes, so keep it on the deduped side.
		return track.Ms, track.Plays, 0, 0
	}
	return 0, 0, track.Ms, track.Plays
}

type mergedName struct {
	name      vaultName
	preSeen   bool
	preMs     int64
	prePlays  int
	postMs    int64
	postPlays int
	days      map[string]vaultDayStats
}

func (m *mergedName) add(name vaultName, month string, cutover string) {
	preMs, prePlays, postMs, postPlays := splitName(name, month, cutover)
	if cutover == "" {
		m.postMs += name.Ms
		m.postPlays += name.Plays
	} else {
		if !m.preSeen {
			m.preMs, m.prePlays, m.preSeen = preMs, prePlays, true
		} else {
			m.preMs = maxInt64(m.preMs, preMs)
			m.prePlays = maxInt(m.prePlays, prePlays)
		}
		m.postMs += postMs
		m.postPlays += postPlays
	}
	if len(name.Days) > 0 {
		if m.days == nil {
			m.days = map[string]vaultDayStats{}
		}
		mergeDayStats(m.days, name.Days, cutover)
	}
	if name.Ms >= m.name.Ms {
		m.name = name
	}
}

func (m *mergedName) total() (int64, int) {
	return m.preMs + m.postMs, m.prePlays + m.postPlays
}

func splitName(name vaultName, month string, cutover string) (preMs int64, prePlays int, postMs int64, postPlays int) {
	if cutover == "" {
		return 0, 0, name.Ms, name.Plays
	}
	if len(name.Days) > 0 {
		var summedMs int64
		var summedPlays int
		for day, stats := range name.Days {
			summedMs += stats.Ms
			summedPlays += stats.Plays
			if day < cutover {
				preMs += stats.Ms
				prePlays += stats.Plays
			} else {
				postMs += stats.Ms
				postPlays += stats.Plays
			}
		}
		if remainder := name.Ms - summedMs; remainder > 0 {
			postMs += remainder
		}
		if remainder := name.Plays - summedPlays; remainder > 0 {
			postPlays += remainder
		}
		return preMs, prePlays, postMs, postPlays
	}
	if month <= cutover[:7] {
		return name.Ms, name.Plays, 0, 0
	}
	return 0, 0, name.Ms, name.Plays
}

func mergeDayStats(into map[string]vaultDayStats, from map[string]vaultDayStats, cutover string) {
	for day, stats := range from {
		existing := into[day]
		if cutover != "" && day < cutover {
			existing.Ms = maxInt64(existing.Ms, stats.Ms)
			existing.Plays = maxInt(existing.Plays, stats.Plays)
		} else {
			existing.Ms += stats.Ms
			existing.Plays += stats.Plays
		}
		into[day] = existing
	}
}

// isoDayKey turns either the phone's day-of-month ("5") or the desktop's ISO
// ("2026-10-05") into one key. Both clients have always sent one of the two.
func isoDayKey(month string, key string) (string, bool) {
	key = strings.TrimSpace(key)
	if len(key) >= 10 && key[4] == '-' {
		if _, ok := dayOfMonth(key); !ok {
			return "", false
		}
		return key, true
	}
	day, err := strconv.Atoi(key)
	if err != nil || day < 1 || day > 31 {
		return "", false
	}
	return fmt.Sprintf("%s-%02d", month, day), true
}

func dayOfMonth(iso string) (int, bool) {
	if len(iso) < 10 {
		return 0, false
	}
	day, err := strconv.Atoi(iso[8:10])
	if err != nil || day < 1 || day > 31 {
		return 0, false
	}
	return day, true
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
