package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func profileMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/profile/{userId}", handleProfileGet)
	mux.HandleFunc("PUT /api/profile/{userId}", handleProfilePut)
	return mux
}

// A downscaled photo is far past one socket read, so it is the body most
// likely to have been clipped. Chosen on the phone, read back on the PC.
func TestProfilePutStoresAPhotoThatArrivesInPieces(t *testing.T) {
	t.Setenv("PROFILE_PATH", filepath.Join(t.TempDir(), "profiles.json"))
	profileLoad()
	mux := profileMux()

	avatar := "data:image/jpeg;base64," + strings.Repeat("A", 60*1024)
	body, err := json.Marshal(map[string]string{"avatar": avatar})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	put := httptest.NewRequest(http.MethodPut, "/api/profile/"+testVaultId, nil)
	put.Body = io.NopCloser(&chunkedBody{data: body, chunk: 1024})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, put)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/api/profile/"+testVaultId, nil)
	grec := httptest.NewRecorder()
	mux.ServeHTTP(grec, get)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", grec.Code)
	}
	var got struct {
		Avatar string `json:"avatar"`
	}
	if err := json.Unmarshal(grec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Avatar != avatar {
		t.Fatalf("avatar came back %d bytes, sent %d", len(got.Avatar), len(avatar))
	}
}

// An absent avatar key must leave the stored picture alone, so a device that
// is only changing something else cannot blank what another one chose.
func TestProfilePutWithoutAvatarKeepsTheStoredOne(t *testing.T) {
	t.Setenv("PROFILE_PATH", filepath.Join(t.TempDir(), "profiles.json"))
	profileLoad()
	mux := profileMux()

	first, err := json.Marshal(map[string]string{"avatar": "https://example.com/a.jpg"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	put := httptest.NewRequest(http.MethodPut, "/api/profile/"+testVaultId, io.NopCloser(strings.NewReader(string(first))))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("first PUT status = %d", rec.Code)
	}

	second := httptest.NewRequest(http.MethodPut, "/api/profile/"+testVaultId, io.NopCloser(strings.NewReader(`{}`)))
	srec := httptest.NewRecorder()
	mux.ServeHTTP(srec, second)
	if srec.Code != http.StatusOK {
		t.Fatalf("second PUT status = %d", srec.Code)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/profile/"+testVaultId, nil)
	grec := httptest.NewRecorder()
	mux.ServeHTTP(grec, get)
	var got struct {
		Avatar string `json:"avatar"`
	}
	if err := json.Unmarshal(grec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Avatar != "https://example.com/a.jpg" {
		t.Fatalf("avatar = %q, want the stored one", got.Avatar)
	}
}
