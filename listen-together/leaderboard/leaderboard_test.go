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

// int64ToID stands in for the hex install ids main.go hands over; the store
// treats them as opaque strings.
func int64ToID(i int) string {
	return string(rune('a'+i%26)) + "-" + string(rune('a'+i/26%26))
}
