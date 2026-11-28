package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/vishnu893-maker/bitify-relay/listen-together/config"
)

// AdminControlState contains only app-wide, non-secret configuration. It is
// intentionally separate from user data and never contains listening history.
type AdminControlState struct {
	Revision            int64           `json:"revision"`
	Maintenance         bool            `json:"maintenance"`
	MaintenanceMessage  string          `json:"maintenanceMessage"`
	Flags               map[string]bool `json:"flags"`
	AnnouncementEnabled bool            `json:"announcementEnabled"`
	AnnouncementTitle   string          `json:"announcementTitle"`
	AnnouncementBody    string          `json:"announcementBody"`
	ServerPreference    string          `json:"serverPreference"`
	// The admin panel's home promo cards, edited without an app release.
	// Kept in the same JSONB payload as everything else so the panel's
	// save-the-whole-state PUT round-trips them untouched.
	PromoCards []AdminPromoCard `json:"promoCards,omitempty"`
	// Bumped when the leaderboard is reset. Clients that see a newer value
	// than they stored forget their high-water marks and republish only
	// listening done since - without it the next presence ping would put
	// everyone's old minutes straight back onto the cleared board.
	ResetEpoch int64     `json:"resetEpoch,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// AdminPromoCard is one remotely edited home card. Everything but id and
// title is optional; the app renders whatever arrives.
type AdminPromoCard struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	CTA      string `json:"cta,omitempty"`
	URL      string `json:"url,omitempty"`
	ImageURL string `json:"imageUrl,omitempty"`
	Accent   string `json:"accent,omitempty"`
}

var adminControlDB *sql.DB

const adminControlsSchema = `
CREATE TABLE IF NOT EXISTS admin_runtime_config (
	config_id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (config_id = 1),
	revision BIGINT NOT NULL DEFAULT 1,
	payload JSONB NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO admin_runtime_config (config_id, revision, payload)
VALUES (1, 1, '{"maintenance":false,"maintenanceMessage":"","flags":{"messages":true,"social":true,"accountSync":true},"announcementEnabled":false,"announcementTitle":"","announcementBody":"","serverPreference":"auto"}'::jsonb)
ON CONFLICT (config_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS fcm_tokens (
	token      TEXT PRIMARY KEY,
	user_id    BIGINT NOT NULL DEFAULT 0,
	platform   TEXT   NOT NULL DEFAULT 'android',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

func adminControlsInit() {
	pg, ok := accountStore.(*postgresAccountStore)
	if !ok || pg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pg.db.ExecContext(ctx, adminControlsSchema); err == nil {
		adminControlDB = pg.db
	}
}

func adminControlAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if config.AdminKey == "" {
		http.NotFound(w, r)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Key")), []byte(config.AdminKey)) != 1 {
		jsonError(w, http.StatusUnauthorized, "unauthorized", "A valid admin key is required.")
		return false
	}
	return true
}

func readAdminControlState(ctx context.Context) (AdminControlState, error) {
	if adminControlDB == nil {
		return AdminControlState{}, errors.New("admin controls are not configured")
	}
	var state AdminControlState
	var payload []byte
	var revision int64
	var updatedAt time.Time
	err := adminControlDB.QueryRowContext(ctx,
		`SELECT revision, payload, updated_at FROM admin_runtime_config WHERE config_id=1`).
		Scan(&revision, &payload, &updatedAt)
	if err != nil {
		return AdminControlState{}, err
	}
	if err := json.Unmarshal(payload, &state); err != nil {
		return AdminControlState{}, err
	}
	if state.ServerPreference == "" {
		state.ServerPreference = "auto"
	}
	state.Revision = revision
	state.UpdatedAt = updatedAt
	return state, nil
}

func handleAdminControls(w http.ResponseWriter, r *http.Request) {
	if !adminControlAuthorized(w, r) {
		return
	}
	if adminControlDB == nil {
		jsonError(w, http.StatusServiceUnavailable, "admin_controls_disabled", "Admin controls require the configured Postgres account store.")
		return
	}
	switch r.Method {
	case http.MethodGet:
		state, err := readAdminControlState(r.Context())
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "server_error", "Could not read admin controls.")
			return
		}
		jsonResponse(w, http.StatusOK, state)
	case http.MethodPut:
		var next AdminControlState
		if !decodeJSONBody(w, r, &next) {
			return
		}
		if !validAdminControlState(next) {
			jsonError(w, http.StatusUnprocessableEntity, "invalid_controls", "The admin control settings are invalid.")
			return
		}
		if err := saveAdminControlState(r.Context(), next); err != nil {
			if errors.Is(err, errAdminControlConflict) {
				jsonError(w, http.StatusConflict, "revision_conflict", "Admin controls changed elsewhere. Refresh before saving.")
				return
			}
			jsonError(w, http.StatusInternalServerError, "server_error", "Could not save admin controls.")
			return
		}
		state, err := readAdminControlState(r.Context())
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "server_error", "Settings saved, but could not reload them.")
			return
		}
		jsonResponse(w, http.StatusOK, state)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// App configuration exposes only fields needed by clients. It does not reveal
// the admin key, internal revision, or database state.
func handleAppConfig(w http.ResponseWriter, r *http.Request) {
	if adminControlDB == nil {
		jsonResponse(w, http.StatusOK, map[string]any{
			"maintenance":        false,
			"maintenanceMessage": "",
			"flags":              map[string]bool{"messages": true, "social": true, "accountSync": true},
			"announcement":       nil,
			"serverPreference":   "auto",
		})
		return
	}
	state, err := readAdminControlState(r.Context())
	if err != nil {
		jsonError(w, http.StatusServiceUnavailable, "config_unavailable", "App configuration is temporarily unavailable.")
		return
	}
	var announcement any
	if state.AnnouncementEnabled {
		announcement = map[string]string{"title": state.AnnouncementTitle, "body": state.AnnouncementBody}
	}
	resp := map[string]any{
		"maintenance":        state.Maintenance,
		"maintenanceMessage": state.MaintenanceMessage,
		"flags":              state.Flags,
		"announcement":       announcement,
		"serverPreference":   state.ServerPreference,
	}
	if len(state.PromoCards) > 0 {
		resp["promoCards"] = state.PromoCards
	}
	if state.ResetEpoch > 0 {
		resp["resetEpoch"] = state.ResetEpoch
	}
	jsonResponse(w, http.StatusOK, resp)
}

var errAdminControlConflict = errors.New("admin controls revision conflict")

func validAdminControlState(state AdminControlState) bool {
	if state.Revision < 1 || len(state.MaintenanceMessage) > 300 || len(state.AnnouncementTitle) > 100 || len(state.AnnouncementBody) > 1000 {
		return false
	}
	if state.ServerPreference != "" && state.ServerPreference != "auto" && state.ServerPreference != "primary" && state.ServerPreference != "backup" {
		return false
	}
	if strings.TrimSpace(state.AnnouncementTitle) == "" && state.AnnouncementEnabled {
		return false
	}
	if strings.TrimSpace(state.AnnouncementBody) == "" && state.AnnouncementEnabled {
		return false
	}
	if len(state.Flags) != 3 {
		return false
	}
	for _, key := range []string{"messages", "social", "accountSync"} {
		if _, ok := state.Flags[key]; !ok {
			return false
		}
	}
	return true
}

func saveAdminControlState(ctx context.Context, state AdminControlState) error {
	expectedRevision := state.Revision
	if state.ServerPreference == "" {
		state.ServerPreference = "auto"
	}
	state.UpdatedAt = time.Time{}
	state.Revision = 0
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	result, err := adminControlDB.ExecContext(ctx,
		`UPDATE admin_runtime_config SET revision=revision+1, payload=$1::jsonb, updated_at=now() WHERE config_id=1 AND revision=$2`,
		string(payload), expectedRevision)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errAdminControlConflict
	}
	return nil
}
