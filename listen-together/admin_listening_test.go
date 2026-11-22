package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vishnu893-maker/bitify-relay/listen-together/config"
)

func TestAggregateListeningDaysIsAnonymousAndRespectsLegacyCutover(t *testing.T) {
	from := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	through := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	accounts := map[string]vaultAccount{
		"opaque-account-a": {
			Cutover: "2026-11-21",
			Devices: map[string][]vaultBucket{
				"phone-one": {{Month: "2026-11", Days: map[string]int64{"20": 10_000, "21": 30_000, "22": 5_000}, Tracks: []vaultTrack{{ID: "track-secret", Title: "Never serialize"}}}},
				"desktop":   {{Month: "2026-11", Days: map[string]int64{"20": 8_000, "21": 20_000, "22": 7_000}}},
			},
		},
		"opaque-account-b": {
			Cutover: "2026-11-01",
			Devices: map[string][]vaultBucket{
				"another-device": {{Month: "2026-11", Days: map[string]int64{"21": 12_000}}},
			},
		},
		"opaque-account-c": {Cutover: "2026-11-01", Devices: map[string][]vaultBucket{"device-c": {{Month: "2026-11", Days: map[string]int64{"21": 1_000}}}}},
		"opaque-account-d": {Cutover: "2026-11-01", Devices: map[string][]vaultBucket{"device-d": {{Month: "2026-11", Days: map[string]int64{"21": 2_000}}}}},
		"opaque-account-e": {Cutover: "2026-11-01", Devices: map[string][]vaultBucket{"device-e": {{Month: "2026-11", Days: map[string]int64{"21": 3_000}}}}},
	}

	got := aggregateListeningDays(accounts, from, through)
	if len(got) != 3 {
		t.Fatalf("got %d days, want 3: %+v", len(got), got)
	}
	want := []*int64{nil, int64Pointer(68_000), nil}
	for i := range want {
		if (got[i].ListeningMs == nil) != (want[i] == nil) {
			t.Errorf("%s withheld = %v, want withheld=%v", got[i].Date, got[i].ListeningMs == nil, want[i] == nil)
			continue
		}
		if want[i] != nil && *got[i].ListeningMs != *want[i] {
			t.Errorf("%s listeningMs = %d, want %d", got[i].Date, *got[i].ListeningMs, *want[i])
		}
	}
}

func TestAggregateListeningDaysIncludesSuppressedDaysWithoutValues(t *testing.T) {
	from := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	through := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	got := aggregateListeningDays(map[string]vaultAccount{}, from, through)
	if len(got) != 2 || got[0].ListeningMs != nil || got[1].ListeningMs != nil {
		t.Fatalf("expected two withheld days below the cohort threshold, got %+v", got)
	}
}

func TestAdminListeningEndpointOnlySerializesAggregateTotals(t *testing.T) {
	oldKey, oldVault, oldStore := config.AdminKey, vaultData, vaultStore
	config.AdminKey = "listening-test-key"
	vaultStore = nil
	defer func() { config.AdminKey, vaultData, vaultStore = oldKey, oldVault, oldStore }()
	vaultData = map[string]vaultAccount{}
	for i := 0; i < adminListeningMinAccounts; i++ {
		vaultData[fmt.Sprintf("account-secret-%d", i)] = vaultAccount{
			Cutover: "2026-11-01",
			Devices: map[string][]vaultBucket{"device-secret": {{
				Month: "2026-11", Days: map[string]int64{time.Now().UTC().Format("2"): 90_000},
				Tracks: []vaultTrack{{ID: "track-secret", Title: "Private song", Artist: "Private artist"}},
			}}},
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/listening", nil)
	req.Header.Set("X-Admin-Key", config.AdminKey)
	rec := httptest.NewRecorder()
	handleAdminListening(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		MinimumCohort int                 `json:"minimumCohort"`
		Days          []adminListeningDay `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.MinimumCohort != adminListeningMinAccounts || len(body.Days) != adminListeningWindowDays {
		t.Fatalf("unexpected aggregate response shape: %+v", body)
	}
	if strings.Contains(rec.Body.String(), "account-secret") || strings.Contains(rec.Body.String(), "device-secret") || strings.Contains(rec.Body.String(), "track-secret") || strings.Contains(rec.Body.String(), "Private song") || strings.Contains(rec.Body.String(), "Private artist") {
		t.Fatalf("response leaked source details: %s", rec.Body.String())
	}
}

func int64Pointer(value int64) *int64 { return &value }
