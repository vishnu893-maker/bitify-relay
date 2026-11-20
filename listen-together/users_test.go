package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// usersMux wires the total-users endpoints (and the account endpoints, so a
// test can register and link) onto fresh in-memory stores. Both rate limiters
// are left nil so a test can call in a loop without tripping them.
func usersMux() *http.ServeMux {
	accountStore = newMemoryAccountStore()
	authLimiter = nil
	userCounter = newMemoryUserCounterStore()
	usersLimiter = nil
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/register", handleAuthRegister)
	mux.HandleFunc("POST /api/auth/login", handleAuthLogin)
	mux.HandleFunc("POST /api/users/install", handleUserInstall)
	mux.HandleFunc("POST /api/users/link", handleUserLink)
	mux.HandleFunc("GET /api/users/count", handleUserCount)
	return mux
}

func userTotal(t *testing.T, mux *http.ServeMux) int64 {
	t.Helper()
	rec, body := doJSON(t, mux, http.MethodGet, "/api/users/count", "", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/users/count = %d, body = %s", rec.Code, rec.Body.String())
	}
	return int64(body["total"].(float64))
}

func install(t *testing.T, mux *http.ServeMux, id string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return doJSON(t, mux, http.MethodPost, "/api/users/install", "", map[string]string{"installId": id}, nil)
}

func link(t *testing.T, mux *http.ServeMux, token, id string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return doJSON(t, mux, http.MethodPost, "/api/users/link", token, map[string]string{"installId": id}, nil)
}

const (
	installPhone = "3f2b8c1e-9a4d-4e6f-8b7a-1c2d3e4f5a6b"
	installPC    = "7c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
	installTab   = "11112222-3333-4444-5555-666677778888"
)

// Case 1: a new install counts once, and reopening it never counts again.
func TestUserInstallCountedOnce(t *testing.T) {
	mux := usersMux()

	rec, body := install(t, mux, installPhone)
	if rec.Code != http.StatusOK {
		t.Fatalf("first install = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body["total"].(float64) != 1 {
		t.Fatalf("total after first install = %v, want 1", body["total"])
	}
	// The same install, many times: still one identity.
	for i := 0; i < 25; i++ {
		if rec, _ := install(t, mux, installPhone); rec.Code != http.StatusOK {
			t.Fatalf("repeat install = %d", rec.Code)
		}
	}
	if got := userTotal(t, mux); got != 1 {
		t.Fatalf("repeated installs inflated the total to %d, want 1", got)
	}

	// A different installation is a different person.
	install(t, mux, installPC)
	if got := userTotal(t, mux); got != 2 {
		t.Fatalf("a second install should be 2, got %d", got)
	}
}

func TestUserInstallRejectsBadID(t *testing.T) {
	mux := usersMux()
	for _, bad := range []string{"", "abc", "not-a-uuid", "12345678-1234-1234-1234-12345678901"} {
		rec, _ := install(t, mux, bad)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("install %q = %d, want 422", bad, rec.Code)
		}
	}
	if got := userTotal(t, mux); got != 0 {
		t.Fatalf("rejected ids changed the total: %d", got)
	}
}

func TestUserLinkRequiresAuth(t *testing.T) {
	mux := usersMux()
	if rec, _ := link(t, mux, "", installPhone); rec.Code != http.StatusUnauthorized {
		t.Fatalf("link without a token = %d, want 401", rec.Code)
	}
	if rec, _ := link(t, mux, "not-a-token", installPhone); rec.Code != http.StatusUnauthorized {
		t.Fatalf("link with a bad token = %d, want 401", rec.Code)
	}
	if got := userTotal(t, mux); got != 0 {
		t.Fatalf("an unauthorized link changed the total: %d", got)
	}
}

// Case 2: a guest logs into an account that was already counted — the account
// does not count twice, and the guest install keeps its own row.
func TestUserGuestLinksExistingAccount(t *testing.T) {
	mux := usersMux()
	token, _, _ := register(t, mux, "ann@example.com", "password123")

	// The account was counted when it was created.
	if got := userTotal(t, mux); got != 1 {
		t.Fatalf("account should count once, got %d", got)
	}
	// The guest install that signs in is a second identity.
	if rec, body := link(t, mux, token, installPhone); rec.Code != http.StatusOK {
		t.Fatalf("link = %d, body = %s", rec.Code, rec.Body.String())
	} else if body["total"].(float64) != 2 {
		t.Fatalf("guest + account should be 2, got %v", body["total"])
	}
	// Linking again — reopening, signing back in — never counts again.
	for i := 0; i < 10; i++ {
		link(t, mux, token, installPhone)
	}
	if got := userTotal(t, mux); got != 2 {
		t.Fatalf("repeated links inflated the total to %d, want 2", got)
	}
}

// Case 3: a guest creates a new account — the account is one more person.
func TestUserNewAccountCounts(t *testing.T) {
	mux := usersMux()
	// The guest is counted first.
	install(t, mux, installPhone)
	if got := userTotal(t, mux); got != 1 {
		t.Fatalf("guest should count once, got %d", got)
	}
	// Creating the account adds the account identity.
	token, _, _ := register(t, mux, "new@example.com", "password123")
	if got := userTotal(t, mux); got != 2 {
		t.Fatalf("guest then a new account should be 2, got %d", got)
	}
	// The client links the install to the account; nothing changes.
	link(t, mux, token, installPhone)
	if got := userTotal(t, mux); got != 2 {
		t.Fatalf("linking after registering changed the total: %d", got)
	}
}

// Case 4: an existing account signing in again — on the same device or a new
// one — never counts the account twice.
func TestUserExistingAccountNeverDoubleCounts(t *testing.T) {
	mux := usersMux()
	token, _, _ := register(t, mux, "bob@example.com", "password123")
	login := func() {
		rec, _ := doJSON(t, mux, http.MethodPost, "/api/auth/login", "",
			map[string]string{"email": "bob@example.com", "password": "password123"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("login = %d", rec.Code)
		}
	}
	login()
	login()
	if got := userTotal(t, mux); got != 1 {
		t.Fatalf("repeat logins counted the account %d times, want 1", got)
	}
	// Same account on two more devices: the account stays one identity.
	link(t, mux, token, installPhone)
	link(t, mux, token, installPC)
	got := userTotal(t, mux)
	if got != 3 {
		t.Fatalf("one account on three devices = %d, want 3 (1 account + 2 installs)", got)
	}
	link(t, mux, token, installPhone)
	link(t, mux, token, installPC)
	if again := userTotal(t, mux); again != got {
		t.Fatalf("re-linking counted again: %d -> %d", got, again)
	}
}

// Case 5: several accounts on one device — each account counts once, the
// install counts once, and switching back and forth changes nothing.
func TestUserMultipleAccountsOneDevice(t *testing.T) {
	mux := usersMux()
	install(t, mux, installPhone)

	firstToken, _, _ := register(t, mux, "one@example.com", "password123")
	secondToken, _, _ := register(t, mux, "two@example.com", "password123")
	// 1 install + 2 accounts.
	if got := userTotal(t, mux); got != 3 {
		t.Fatalf("install + two accounts = %d, want 3", got)
	}
	link(t, mux, firstToken, installPhone)
	link(t, mux, secondToken, installPhone)
	if got := userTotal(t, mux); got != 3 {
		t.Fatalf("switching accounts changed the total to %d, want 3", got)
	}
}

// The count endpoint reflects the tally the write paths built.
func TestUserCountEndpoint(t *testing.T) {
	mux := usersMux()
	install(t, mux, installPhone)
	install(t, mux, installPC)
	install(t, mux, installTab)
	if got := userTotal(t, mux); got != 3 {
		t.Fatalf("three installs = %d, want 3", got)
	}
}

// With no store configured the routes answer 503 rather than pretending zero.
func TestUserCountDisabled(t *testing.T) {
	saved := userCounter
	userCounter = nil
	defer func() { userCounter = saved }()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/users/install", handleUserInstall)
	mux.HandleFunc("GET /api/users/count", handleUserCount)

	if rec, _ := doJSON(t, mux, http.MethodGet, "/api/users/count", "", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("count with no store = %d, want 503", rec.Code)
	}
}
