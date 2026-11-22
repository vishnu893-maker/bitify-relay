package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KabirSinghBhatia/BitChord/backend/config"
)

func TestAdminControlsRequireKey(t *testing.T) {
	oldKey := config.AdminKey
	config.AdminKey = "admin-test-secret"
	defer func() { config.AdminKey = oldKey }()

	for _, tc := range []struct {
		name string
		key  string
		want int
	}{
		{name: "missing key", want: http.StatusUnauthorized},
		{name: "wrong key", key: "wrong", want: http.StatusUnauthorized},
		{name: "correct key but no database", key: "admin-test-secret", want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldDB := adminControlDB
			adminControlDB = nil
			defer func() { adminControlDB = oldDB }()

			req := httptest.NewRequest(http.MethodGet, "/api/admin/controls", nil)
			if tc.key != "" {
				req.Header.Set("X-Admin-Key", tc.key)
			}
			rec := httptest.NewRecorder()
			handleAdminControls(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestAdminControlsAreHiddenWhenNoKeyIsConfigured(t *testing.T) {
	oldKey := config.AdminKey
	config.AdminKey = ""
	defer func() { config.AdminKey = oldKey }()

	req := httptest.NewRequest(http.MethodGet, "/api/admin/controls", nil)
	rec := httptest.NewRecorder()
	handleAdminControls(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestPublicAppConfigWithoutDatabaseContainsOnlySafeDefaults(t *testing.T) {
	oldDB := adminControlDB
	adminControlDB = nil
	defer func() { adminControlDB = oldDB }()

	req := httptest.NewRequest(http.MethodGet, "/api/app/config", nil)
	rec := httptest.NewRecorder()
	handleAppConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, expected := range []string{`"maintenance":false`, `"messages":true`, `"social":true`, `"accountSync":true`, `"announcement":null`} {
		if !strings.Contains(body, expected) {
			t.Errorf("public config missing %s: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"adminKey", "revision", "updatedAt", "database"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("public config leaked %q: %s", forbidden, body)
		}
	}
}

func TestValidAdminControlState(t *testing.T) {
	state := AdminControlState{
		Revision: 1,
		Flags:    map[string]bool{"messages": true, "social": true, "accountSync": false},
	}
	if !validAdminControlState(state) {
		t.Fatal("expected complete default state to be valid")
	}
	state.Flags["unknown"] = true
	if validAdminControlState(state) {
		t.Fatal("unknown feature flag should be rejected")
	}
	state.Flags = map[string]bool{"messages": true, "social": true, "accountSync": true}
	state.AnnouncementEnabled = true
	if validAdminControlState(state) {
		t.Fatal("enabled announcement without text should be rejected")
	}
	state.AnnouncementTitle = "Maintenance"
	state.AnnouncementBody = "We are working on the service."
	if !validAdminControlState(state) {
		t.Fatal("enabled announcement with content should be accepted")
	}
}
