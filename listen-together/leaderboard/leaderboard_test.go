package leaderboard

import (
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
	if snap.Leaders[0].Name != "Aarav" || snap.Leaders[0].Songs != 10 {
		t.Fatalf("first = %+v, want Aarav/10 (99-song unnamed listener stays hidden)", snap.Leaders[0])
	}
	if snap.Leaders[1].Name != "Badname" || snap.Leaders[1].Songs != 7 {
		t.Fatalf("second = %+v, want Badname/7", snap.Leaders[1])
	}
}

func TestPersistenceAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.json")
	s := NewStore(path)
	s.Record("a", "Riya", 42, 1)
	s.Flush()

	reopened := NewStore(path)
	snap := reopened.Snapshot(10, 2)
	if len(snap.Leaders) != 1 || snap.Leaders[0].Name != "Riya" || snap.Leaders[0].Songs != 42 {
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

// int64ToID stands in for the hex install ids main.go hands over; the store
// treats them as opaque strings.
func int64ToID(i int) string {
	return string(rune('a'+i%26)) + "-" + string(rune('a'+i/26%26))
}
