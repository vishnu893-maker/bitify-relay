package main

import (
	"net/http"
	"time"
)

const (
	adminListeningWindowDays  = 30
	adminListeningMinAccounts = 5
)

type adminListeningDay struct {
	Date        string `json:"date"`
	ListeningMs *int64 `json:"listeningMs"`
}

// handleAdminListening returns cross-account daily totals only. It never
// serializes vault IDs, device IDs, track metadata, or per-account values.
// Days below the minimum cohort size are withheld to reduce re-identification.
func handleAdminListening(w http.ResponseWriter, r *http.Request) {
	if !adminControlAuthorized(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	now := time.Now().UTC()
	from := now.AddDate(0, 0, -(adminListeningWindowDays - 1))
	vaultMu.Lock()
	store := vaultStore
	if store == nil {
		// Hold the lock for the read: in-memory vault updates replace and mutate
		// nested device data while request handlers are active.
		daily := aggregateListeningDays(vaultData, from, now)
		vaultMu.Unlock()
		writeAdminListeningResponse(w, daily, from, now)
		return
	}
	vaultMu.Unlock()

	// A fresh read includes writes made through the other relay instance,
	// which shares this Postgres database but has its own process memory.
	accounts, err := store.Load()
	if err != nil {
		jsonError(w, http.StatusServiceUnavailable, "listening_unavailable", "Aggregate listening data is temporarily unavailable.")
		return
	}
	daily := aggregateListeningDays(accounts, from, now)
	writeAdminListeningResponse(w, daily, from, now)
}

func writeAdminListeningResponse(w http.ResponseWriter, daily []adminListeningDay, from, now time.Time) {

	var totalMs int64
	for _, day := range daily {
		if day.ListeningMs != nil {
			totalMs += *day.ListeningMs
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"periodDays":       adminListeningWindowDays,
		"minimumCohort":    adminListeningMinAccounts,
		"from":             from.Format("2006-01-02"),
		"through":          now.Format("2006-01-02"),
		"totalListeningMs": totalMs,
		"days":             daily,
		"serverMs":         now.UnixMilli(),
	})
}

// aggregateListeningDays mirrors vaultMerge's day rule without merging or
// materializing any track, artist, album, or device details. Legacy days before
// each account's cutover use max-per-day; days after cutover are additive.
func aggregateListeningDays(accounts map[string]vaultAccount, from, through time.Time) []adminListeningDay {
	first := from.UTC().Format("2006-01-02")
	last := through.UTC().Format("2006-01-02")
	perDay := make(map[string]int64, adminListeningWindowDays)
	accountCounts := make(map[string]int, adminListeningWindowDays)

	for _, account := range accounts {
		preCutover := make(map[string]int64)
		postCutover := make(map[string]int64)
		for _, buckets := range account.Devices {
			for _, bucket := range buckets {
				if !validMonth(bucket.Month) {
					continue
				}
				for dayKey, milliseconds := range bucket.Days {
					date, ok := isoDayKey(bucket.Month, dayKey)
					parsed, parseErr := time.Parse("2006-01-02", date)
					if !ok || parseErr != nil || parsed.Format("2006-01") != bucket.Month || date < first || date > last || milliseconds <= 0 {
						continue
					}
					if account.Cutover != "" && date < account.Cutover {
						if milliseconds > preCutover[date] {
							preCutover[date] = milliseconds
						}
					} else {
						postCutover[date] += milliseconds
					}
				}
			}
		}
		accountDays := make(map[string]int64, len(preCutover)+len(postCutover))
		for date, milliseconds := range preCutover {
			accountDays[date] += milliseconds
		}
		for date, milliseconds := range postCutover {
			accountDays[date] += milliseconds
		}
		for date, milliseconds := range accountDays {
			if milliseconds > 0 {
				perDay[date] += milliseconds
				accountCounts[date]++
			}
		}
	}

	result := make([]adminListeningDay, 0, adminListeningWindowDays)
	for date := time.Date(from.UTC().Year(), from.UTC().Month(), from.UTC().Day(), 0, 0, 0, 0, time.UTC); !date.After(through.UTC()); date = date.AddDate(0, 0, 1) {
		key := date.Format("2006-01-02")
		day := adminListeningDay{Date: key}
		if accountCounts[key] >= adminListeningMinAccounts {
			value := perDay[key]
			day.ListeningMs = &value
		}
		result = append(result, day)
	}
	return result
}
