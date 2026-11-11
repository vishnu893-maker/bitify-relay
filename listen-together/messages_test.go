package main

import "testing"

func TestSniffMime(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0}, "image/jpeg"},
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0}, "image/png"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"wav", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), "audio/wav"},
		{"mp4 audio", []byte("\x00\x00\x00\x18ftypM4A \x00\x00"), "audio/mp4"},
		{"ogg", []byte("OggS\x00\x02\x00\x00"), "audio/ogg"},
		{"webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x01}, "audio/webm"},
		{"exe rejected", []byte("MZ\x90\x00\x03\x00\x00\x00\x04"), ""},
		{"html rejected", []byte("<html><script>alert(1)</script>"), ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		if got := sniffMime(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// The types the relay takes, and the sizes it takes them at. A type that is
// not in this map is refused whatever the client claims about it.
func TestMediaLimits(t *testing.T) {
	for _, mime := range []string{"image/jpeg", "image/png", "image/webp", "audio/mp4", "audio/mpeg", "audio/ogg", "audio/webm", "audio/wav"} {
		limit, ok := mediaLimits[mime]
		if !ok {
			t.Errorf("%s should be allowed", mime)
			continue
		}
		if limit <= 0 {
			t.Errorf("%s should have a positive limit", mime)
		}
	}
	if _, ok := mediaLimits["application/octet-stream"]; ok {
		t.Error("an unknown type must not be allowed")
	}
	if _, ok := mediaLimits["text/html"]; ok {
		t.Error("html must not be allowed")
	}
}

func TestValidUsername(t *testing.T) {
	good := []string{"abc", "Bit.ify_01", "a_b"}
	bad := []string{"", "ab", "has space", "emoji😀", "this_name_is_way_too_long_for_it", "a@b.com"}
	for _, s := range good {
		if !validUsername(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range bad {
		if validUsername(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestPairOrdersIDs(t *testing.T) {
	if lo, hi := pair(9, 3); lo != 3 || hi != 9 {
		t.Fatalf("got %d,%d", lo, hi)
	}
	if lo, hi := pair(3, 9); lo != 3 || hi != 9 {
		t.Fatalf("got %d,%d", lo, hi)
	}
}

func TestClipRunes(t *testing.T) {
	if got := clip("  héllo wörld  ", 5); got != "héllo" {
		t.Fatalf("got %q", got)
	}
}
