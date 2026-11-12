package leaderboard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordAndSnapshot(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "board.json"))
	// Songs only go up: a second, lower report cannot pull A backwards.
	s.Record("a", "Aarav", 10, 1)
	s.Record("a", "Aarav", 4, 2)
	// A blank ping keeps the entry and its count, but a blank name stays off the board.
	s.Record("b", "", 99, 3)
	// Unnamed installs total but never show.
	s.Record("c", "", 5, 4)
	// Names are sanitized: control characters dropped, length capped.
	s.Record("d", "  Bad\x00name  ", 7, 5)

	snap := s.Snapshot(25, 6)
	if snap.Listeners != 4 {
		t.Fatalf("listeners = %d, want 4", snap.Listeners)
	}
	// "b" listened plenty but never chose a name: counted, never shown.
	if len(snap.Leaders) != 2 {
		t.Fatalf("leaders = %d (%v), want 2 named entries", len(snap.Leaders), snap.Leaders)
	}
	if snap.Leaders[0].Name != "Aarav" || snap.Leaders[0].Ms != 10 {
		t.Fatalf("first = %+v, want Aarav/10ms (99-song unnamed listener stays hidden)", snap.Leaders[0])
	}
	if snap.Leaders[1].Name != "Badname" || snap.Leaders[1].Ms != 7 {
		t.Fatalf("second = %+v, want Badname/7ms", snap.Leaders[1])
	}
}

func TestPersistenceAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.json")
	s := NewStore(path)
	s.Record("a", "Riya", 42, 1)
	s.Flush()

	reopened := NewStore(path)
	snap := reopened.Snapshot(10, 2)
	if len(snap.Leaders) != 1 || snap.Leaders[0].Name != "Riya" || snap.Leaders[0].Ms != 42 {
		t.Fatalf("reopened board = %+v, want Riya/42", snap.Leaders)
	}
}

func TestTopLimit(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "board.json"))
	for i := 0; i < 40; i++ {
		s.Record(int64ToID(i), "Listener", int64(i+1), int64(i))
	}
	if got := len(s.Snapshot(25, 0).Leaders); got != 25 {
		t.Fatalf("top = %d, want capped at 25", got)
	}
}

// A named account with no minutes yet is still a row, so a fresh install can
// see itself on the board instead of vanishing until its first full song.
func TestZeroMinuteNamedRowIsListed(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "board.json"))
	s.Record("a", "New Listener", 0, 1)
	s.Record("b", "Busy Listener", 60_000, 2)

	snap := s.Snapshot(25, 3)
	if len(snap.Leaders) != 2 {
		t.Fatalf("leaders = %+v, want both the 0 and the 60000 minute rows", snap.Leaders)
	}
	if snap.Leaders[0].Name != "Busy Listener" {
		t.Fatalf("first = %+v, want Busy Listener first", snap.Leaders[0])
	}
	if snap.Leaders[1].Name != "New Listener" || snap.Leaders[1].Ms != 0 {
		t.Fatalf("last = %+v, want New Listener at 0", snap.Leaders[1])
	}
}

// A board saved by the old server holds play counts under "songs" and a
// placeholder row named "Test Listener". Neither may survive a load.
func TestLegacyAndDemoRowsAreDroppedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.json")
	raw := `{
		"a": {"name": "Test Listener", "songs": 42, "lastMs": 1},
		"b": {"name": "vishnu", "songs": 1, "lastMs": 1},
		"c": {"name": "Riya", "ms": 500, "lastMs": 2},
		"d": {"name": "Zero", "ms": 0, "lastMs": 3}
	}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write board: %v", err)
	}

	s := NewStore(path)
	snap := s.Snapshot(25, 4)
	if snap.Listeners != 2 {
		t.Fatalf("listeners = %d, want the two non-legacy rows", snap.Listeners)
	}
	if len(snap.Leaders) != 2 {
		t.Fatalf("leaders = %+v, want Riya and Zero", snap.Leaders)
	}
	if snap.Leaders[0].Name != "Riya" || snap.Leaders[0].Ms != 500 {
		t.Fatalf("first = %+v, want Riya/500", snap.Leaders[0])
	}
	if snap.Leaders[1].Name != "Zero" || snap.Leaders[1].Ms != 0 {
		t.Fatalf("second = %+v, want Zero/0", snap.Leaders[1])
	}

	// And the demo name cannot be recorded back onto the board.
	s.Record("e", "Test Listener", 999_000, 5)
	for _, entry := range s.Snapshot(25, 6).Leaders {
		if entry.Name == "Test Listener" {
			t.Fatalf("demo row came back: %+v", entry)
		}
	}
}

// A signed-in account and the install it signed in on are one person, and the
// board must show one row for them — never the install row and the account
// row side by side.
func TestRecordAccountFoldsInstallRow(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "board.json"))
	// Before signing in, the install reported under its own id.
	s.Record("install-1", "Vishu", 13_552_424, 1)
	// The same person now signs in: the account key arrives with the heartbeat.
	s.RecordAccount("acct-vishnu", "install-1", "hacker", 78_608_434, 2)

	snap := s.Snapshot(25, 1)
	if len(snap.Leaders) != 1 {
		t.Fatalf("leaders = %+v, want one row for one person", snap.Leaders)
	}
	// The larger total wins; the two rows report the same account's listening,
	// so adding them would count the same evening twice.
	if snap.Leaders[0].Ms != 78_608_434 {
		t.Fatalf("ms = %d, want 78608434 (the larger total, not the sum)", snap.Leaders[0].Ms)
	}
	if snap.Leaders[0].Name != "hacker" {
		t.Fatalf("name = %q, want the signed-in nickname", snap.Leaders[0].Name)
	}

	// A later heartbeat from that install without an account key — signed out,
	// or from an older build — still lands on the same row.
	s.Record("install-1", "hacker", 80_000_000, 3)
	snap = s.Snapshot(25, 1)
	if len(snap.Leaders) != 1 {
		t.Fatalf("leaders = %+v, want still one row", snap.Leaders)
	}
	if snap.Leaders[0].Ms != 80_000_000 {
		t.Fatalf("ms = %d, want 80000000", snap.Leaders[0].Ms)
	}
}

// The fold survives a restart: the board's file remembers which installs
// belong to which account.
func TestRecordAccountSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.json")
	s := NewStore(path)
	s.Record("install-2", "Vishu", 1000, 1)
	s.RecordAccount("acct-vishnu", "install-2", "hacker", 2000, 2)
	s.Flush()

	reloaded := NewStore(path)
	reloaded.Record("install-2", "hacker", 3000, 3)
	snap := reloaded.Snapshot(25, 1)
	if len(snap.Leaders) != 1 {
		t.Fatalf("leaders = %+v, want one row after a reload", snap.Leaders)
	}
	if snap.Leaders[0].Ms != 3000 {
		t.Fatalf("ms = %d, want 3000", snap.Leaders[0].Ms)
	}
}

// A person who played signed out and later signed in is still one person, so
// the board must show one row for them. Their key changes with the sign-in —
// from the hash of their Google login to the account id the server issued —
// and the row the old key left behind has to go in with the new one. This is
// the double name on the board: the same listener under two names, because
// two rows were kept for one account.
func TestRecordAccountFoldsTheRowTheKeyChangeLeftBehind(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "board.json"))
	// Signed out: the client reported under the hash of its Google login.
	s.RecordAccount("google-hash", "install-3", "Hey", 2_868_990, 1)
	// Signed in: the same install now reports under the account id.
	s.RecordAccount("acct-vishnu", "install-3", "hacker", 80_967_936, 2)

	snap := s.Snapshot(25, 3)
	if len(snap.Leaders) != 1 {
		t.Fatalf("leaders = %+v, want one row for one person", snap.Leaders)
	}
	// The larger total wins; the two rows report the same listener's minutes,
	// so adding them would count the same evening twice.
	if snap.Leaders[0].Ms != 80_967_936 {
		t.Fatalf("ms = %d, want 80967936 (the larger total, not the sum)", snap.Leaders[0].Ms)
	}
	// One name: the one the listener is using now.
	if snap.Leaders[0].Name != "hacker" {
		t.Fatalf("name = %q, want the signed-in nickname", snap.Leaders[0].Name)
	}

	// And the install stays claimed, so a later signed-out heartbeat from it
	// lands on the same row rather than opening a third one.
	s.Record("install-3", "hacker", 81_000_000, 4)
	snap = s.Snapshot(25, 5)
	if len(snap.Leaders) != 1 {
		t.Fatalf("leaders = %+v, want still one row", snap.Leaders)
	}
}

// The board survives a restart through its durable store: the rows and the
// installs that belong to them are both remembered, so the fold still works
// after the process is replaced.
func TestBoardRoundTripsThroughItsRows(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "board.json"))
	s.RecordAccount("google-hash", "install-4", "Hey", 1000, 1)
	rows := s.Rows()
	if len(rows) != 1 || rows[0].ID != "google-hash" || len(rows[0].Installs) != 1 {
		t.Fatalf("rows = %+v, want the google-hash row with its install", rows)
	}

	reloaded := NewStore(filepath.Join(t.TempDir(), "board.json"))
	reloaded.ReplaceRows(rows)
	reloaded.RecordAccount("acct-vishnu", "install-4", "hacker", 2000, 2)
	snap := reloaded.Snapshot(25, 3)
	if len(snap.Leaders) != 1 {
		t.Fatalf("leaders = %+v, want one row after a reload", snap.Leaders)
	}
	if snap.Leaders[0].Name != "hacker" || snap.Leaders[0].Ms != 2000 {
		t.Fatalf("leaders[0] = %+v, want hacker/2000", snap.Leaders[0])
	}
}

// int64ToID stands in for the hex install ids main.go hands over; the store
// treats them as opaque strings.
func int64ToID(i int) string {
	return string(rune('a'+i%26)) + "-" + string(rune('a'+i/26%26))
}
