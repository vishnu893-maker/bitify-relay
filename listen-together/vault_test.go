package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A vault id the server accepts: exactly 32 lowercase hex.
const testVaultId = "0123456789abcdef0123456789abcdef"

// chunkedBody hands a body over the way a socket does — a piece at a time.
// Read is free to return short, and behind Render's proxy it does, which is
// what used to truncate a large vault into bad_buckets.
type chunkedBody struct {
	data  []byte
	chunk int
}

func (c *chunkedBody) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(c.chunk, len(p), len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func vaultMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vault/{userId}", handleVaultGet)
	mux.HandleFunc("PUT /api/vault/{userId}", handleVaultPut)
	return mux
}

func putVault(t *testing.T, mux *http.ServeMux, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	put := httptest.NewRequest(http.MethodPut, "/api/vault/"+testVaultId, nil)
	put.Body = io.NopCloser(&chunkedBody{data: body, chunk: 1024})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, put)
	return rec
}

func putPush(t *testing.T, mux *http.ServeMux, deviceID string, buckets []vaultBucket) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(vaultPush{DeviceID: deviceID, Buckets: buckets})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	return putVault(t, mux, body)
}

func getVault(t *testing.T, mux *http.ServeMux) ([]vaultBucket, int) {
	t.Helper()
	get := httptest.NewRequest(http.MethodGet, "/api/vault/"+testVaultId, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var buckets []vaultBucket
	if err := json.Unmarshal(rec.Body.Bytes(), &buckets); err != nil {
		t.Fatalf("decode vault: %v", err)
	}
	return buckets, rec.Code
}

// todayParts returns the day the server uses as the cutover for a fresh
// account, so an additive test never lands on the deduped side of the line.
func todayParts() (iso, month, day string) {
	iso = time.Now().Format("2006-01-02")
	return iso, iso[:7], iso[8:10]
}

func findBucket(t *testing.T, buckets []vaultBucket, month string) vaultBucket {
	t.Helper()
	for _, bucket := range buckets {
		if bucket.Month == month {
			return bucket
		}
	}
	t.Fatalf("no bucket for %s in %+v", month, buckets)
	return vaultBucket{}
}

func TestVaultPutStoresABodyThatArrivesInPieces(t *testing.T) {
	t.Setenv("VAULT_PATH", filepath.Join(t.TempDir(), "vault.json"))
	vaultLoad()
	mux := vaultMux()

	today, month, day := todayParts()
	// Comfortably past anything one socket read would return.
	longID := strings.Repeat("a", 40*1024)
	body, err := json.Marshal(vaultPush{
		DeviceID: "phone-0001",
		Buckets: []vaultBucket{{
			Month: month,
			Days:  map[string]int64{day: 60_000},
			Tracks: []vaultTrack{{
				ID:    longID,
				Ms:    60_000,
				Plays: 1,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := putVault(t, mux, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var ok struct {
		Bytes int `json:"bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ok.Bytes != len(body) {
		t.Fatalf("stored %d bytes, sent %d — the body was truncated", ok.Bytes, len(body))
	}

	// And it reads back whole, which is what a second device pulls.
	buckets, _ := getVault(t, mux)
	bucket := findBucket(t, buckets, month)
	if got := bucket.DaysISO[today]; got != 60_000 {
		t.Fatalf("daysIso[%s] = %d, want 60000", today, got)
	}
	if len(bucket.Tracks) != 1 || bucket.Tracks[0].Ms != 60_000 {
		t.Fatalf("track did not survive the round trip: %+v", bucket.Tracks)
	}
}

func TestVaultPutStillRefusesAnOversizedBody(t *testing.T) {
	t.Setenv("VAULT_PATH", filepath.Join(t.TempDir(), "vault.json"))
	vaultLoad()
	mux := vaultMux()

	blob := strings.Repeat("a", vaultMaxUserBytes+2048)
	body, err := json.Marshal([]map[string]any{{"month": "2026-10", "blob": blob}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := putVault(t, mux, body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

// The point of the rewrite: two devices listening on the same day add up
// instead of the busier one winning.
func TestVaultAddsTwoDevicesOnTheSameDay(t *testing.T) {
	t.Setenv("VAULT_PATH", filepath.Join(t.TempDir(), "vault.json"))
	vaultLoad()
	mux := vaultMux()
	today, month, day := todayParts()

	phone := putPush(t, mux, "phone-0001", []vaultBucket{{
		Month: month,
		Days:  map[string]int64{day: 60_000},
		Tracks: []vaultTrack{{
			ID:    "track-1",
			Ms:    60_000,
			Plays: 1,
			Days:  map[string]vaultDayStats{today: {Ms: 60_000, Plays: 1}},
		}},
	}})
	if phone.Code != http.StatusOK {
		t.Fatalf("phone PUT = %d, %s", phone.Code, phone.Body.String())
	}

	desktop := putPush(t, mux, "desktop-0001", []vaultBucket{{
		Month: month,
		Days:  map[string]int64{day: 30_000},
		Tracks: []vaultTrack{{
			ID:    "track-1",
			Ms:    30_000,
			Plays: 1,
			Days:  map[string]vaultDayStats{today: {Ms: 30_000, Plays: 1}},
		}},
	}})
	if desktop.Code != http.StatusOK {
		t.Fatalf("desktop PUT = %d, %s", desktop.Code, desktop.Body.String())
	}

	buckets, _ := getVault(t, mux)
	bucket := findBucket(t, buckets, month)
	if got := bucket.DaysISO[today]; got != 90_000 {
		t.Fatalf("daysIso[%s] = %d, want 90000 (60000 + 30000)", today, got)
	}
	if len(bucket.Tracks) != 1 {
		t.Fatalf("want one merged track, got %+v", bucket.Tracks)
	}
	track := bucket.Tracks[0]
	if track.Ms != 90_000 || track.Plays != 2 {
		t.Fatalf("merged track = %d ms / %d plays, want 90000 / 2", track.Ms, track.Plays)
	}
	if got := track.Days[today]; got.Ms != 90_000 || got.Plays != 2 {
		t.Fatalf("merged track day = %+v, want 90000 ms / 2 plays", got)
	}
}

// An old vault file held one max-merged copy. Its days before the cutover stay
// deduped; days from the cutover on are summed with every device.
func TestVaultMigratesAnOldMergedFileAtTheCutover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	t.Setenv("VAULT_PATH", path)
	today, month, day := todayParts()
	pastMonth := time.Now().AddDate(0, -1, 0).Format("2006-01")
	pastDay := pastMonth + "-01"

	legacy := map[string]any{
		testVaultId: map[string]any{
			"updatedAt": 123,
			"buckets": []vaultBucket{
				{Month: pastMonth, Days: map[string]int64{"1": 1_000}},
				{Month: month, Days: map[string]int64{day: 2_000}},
			},
		},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	vaultLoad()
	mux := vaultMux()

	buckets, _ := getVault(t, mux)
	if got := findBucket(t, buckets, pastMonth).DaysISO[pastDay]; got != 1_000 {
		t.Fatalf("legacy past day = %d, want 1000", got)
	}
	if got := findBucket(t, buckets, month).DaysISO[today]; got != 2_000 {
		t.Fatalf("legacy today = %d, want 2000", got)
	}

	rec := putPush(t, mux, "phone-0001", []vaultBucket{
		{Month: pastMonth, Days: map[string]int64{"1": 4_000}},
		{Month: month, Days: map[string]int64{day: 3_000}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, %s", rec.Code, rec.Body.String())
	}

	buckets, _ = getVault(t, mux)
	// Before the cutover the old copy may already hold this evening's minutes,
	// so the larger copy wins rather than the sum.
	if got := findBucket(t, buckets, pastMonth).DaysISO[pastDay]; got != 4_000 {
		t.Fatalf("pre-cutover day = %d, want max(1000, 4000) = 4000", got)
	}
	// From the cutover on, each device's own copy is real and adds up.
	if got := findBucket(t, buckets, month).DaysISO[today]; got != 5_000 {
		t.Fatalf("cutover day = %d, want 2000 + 3000 = 5000", got)
	}
}

// A client that has not been updated still sends the bare array. It is kept as
// a device so its minutes are not lost, and later additive devices add to it.
func TestVaultAcceptsTheOldBareArray(t *testing.T) {
	t.Setenv("VAULT_PATH", filepath.Join(t.TempDir(), "vault.json"))
	vaultLoad()
	mux := vaultMux()
	today, month, day := todayParts()

	body, err := json.Marshal([]vaultBucket{{
		Month: month,
		Days:  map[string]int64{day: 5_000},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := putVault(t, mux, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy PUT = %d, %s", rec.Code, rec.Body.String())
	}
	var ok struct {
		Devices int `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ok.Devices != 1 {
		t.Fatalf("devices = %d, want 1", ok.Devices)
	}

	if got := findBucket(t, mustGet(t, mux), month).DaysISO[today]; got != 5_000 {
		t.Fatalf("legacy day = %d, want 5000", got)
	}

	putPush(t, mux, "phone-0001", []vaultBucket{{
		Month: month,
		Days:  map[string]int64{day: 3_000},
	}})
	if got := findBucket(t, mustGet(t, mux), month).DaysISO[today]; got != 8_000 {
		t.Fatalf("legacy + new = %d, want 8000", got)
	}
}

func mustGet(t *testing.T, mux *http.ServeMux) []vaultBucket {
	t.Helper()
	buckets, _ := getVault(t, mux)
	return buckets
}

// A client that names itself gets the merged account and its own last upload
// in one answer, which is how it knows which local minutes are still unsynced.
func TestVaultGetReturnsThisDevicesCopyBesideTheMerge(t *testing.T) {
	t.Setenv("VAULT_PATH", filepath.Join(t.TempDir(), "vault.json"))
	vaultLoad()
	mux := vaultMux()
	today, month, day := todayParts()

	putPush(t, mux, "phone-0001", []vaultBucket{{Month: month, Days: map[string]int64{day: 60_000}}})
	putPush(t, mux, "desktop-0001", []vaultBucket{{Month: month, Days: map[string]int64{day: 30_000}}})

	get := httptest.NewRequest(http.MethodGet, "/api/vault/"+testVaultId+"?device=phone-0001", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var answer struct {
		Merged []vaultBucket `json:"merged"`
		Device []vaultBucket `json:"device"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := findBucket(t, answer.Merged, month).DaysISO[today]; got != 90_000 {
		t.Fatalf("merged day = %d, want 90000", got)
	}
	// The device copy is the exact upload, not the merged shape: it keeps the
	// client's own day keys so the client can subtract it from local storage.
	if got := findBucket(t, answer.Device, month).Days[day]; got != 60_000 {
		t.Fatalf("device day = %d, want this install's own 60000", got)
	}
}

func TestVaultGetIsEmptyBeforeAnyPush(t *testing.T) {
	t.Setenv("VAULT_PATH", filepath.Join(t.TempDir(), "vault.json"))
	vaultLoad()
	mux := vaultMux()

	get := httptest.NewRequest(http.MethodGet, "/api/vault/"+testVaultId, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, get)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A history too big for one request is sent in pieces, each piece merged into
// what the server already has. The months a piece does not mention are still
// this device's and must stay where they are — otherwise a long history is
// lost the moment it grows past the request limit, which is exactly how a
// listener's months went missing.
func TestVaultMergeKeepsTheMonthsTheBodyDidNotMention(t *testing.T) {
	t.Setenv("VAULT_PATH", filepath.Join(t.TempDir(), "vault.json"))
	vaultLoad()
	mux := vaultMux()
	_, month, day := todayParts()
	other := "2026-10"

	// The first piece: October.
	putPush(t, mux, "phone-0001", []vaultBucket{{Month: other, Days: map[string]int64{"05": 60_000}}})
	// The second piece: November, sent as a merge.
	body, err := json.Marshal(vaultPush{
		DeviceID: "phone-0001",
		Merge:    true,
		Buckets:  []vaultBucket{{Month: month, Days: map[string]int64{day: 30_000}}},
	})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	if rec := putVault(t, mux, body); rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}

	buckets := mustGet(t, mux)
	if len(buckets) != 2 {
		t.Fatalf("months = %+v, want both October and November", buckets)
	}
	// The merged answer keys its days by the day of the month.
	if got := findBucket(t, buckets, other).Days["5"]; got != 60_000 {
		t.Fatalf("October day = %d, want the piece the second body did not mention", got)
	}
	if got := findBucket(t, buckets, month).Days[day]; got != 30_000 {
		t.Fatalf("November day = %d, want 30000", got)
	}
}

// A plain push still replaces the device's copy: a client that cleared its
// own history says so, and the server has to believe it.
func TestVaultPlainPushStillReplaces(t *testing.T) {
	t.Setenv("VAULT_PATH", filepath.Join(t.TempDir(), "vault.json"))
	vaultLoad()
	mux := vaultMux()
	_, month, day := todayParts()

	putPush(t, mux, "phone-0001", []vaultBucket{{Month: month, Days: map[string]int64{day: 60_000}}})
	putPush(t, mux, "phone-0001", []vaultBucket{{Month: month, Days: map[string]int64{day: 10_000}}})

	buckets := mustGet(t, mux)
	if len(buckets) != 1 {
		t.Fatalf("months = %+v, want one month", buckets)
	}
	if got := findBucket(t, buckets, month).Days[day]; got != 10_000 {
		t.Fatalf("day = %d, want 10000 (the second push replaced the first)", got)
	}
}
