package rules

import "testing"

func TestBlocklistMatch(t *testing.T) {
	b := NewBlocklist([]string{"example.com", "*.ads.net"})

	cases := []struct {
		name  string
		entry string
		ok    bool
	}{
		{"example.com.", "example.com", true},
		{"a.b.example.com.", "example.com", true},
		{"EXAMPLE.COM.", "example.com", true},
		{"notexample.com.", "", false}, // не совпадает по границе метки
		{"ads.net.", "ads.net", true},
		{"tracker.ads.net.", "ads.net", true},
		{"other.net.", "", false},
	}
	for _, c := range cases {
		entry, ok := b.Match(c.name)
		if ok != c.ok || entry != c.entry {
			t.Errorf("Match(%q) = (%q,%v), want (%q,%v)", c.name, entry, ok, c.entry, c.ok)
		}
	}
}

func TestBlocklistMostSpecificEntry(t *testing.T) {
	b := NewBlocklist([]string{"example.com", "ads.example.com"})
	entry, ok := b.Match("x.ads.example.com.")
	if !ok || entry != "ads.example.com" {
		t.Fatalf("Match = (%q,%v), want (ads.example.com,true)", entry, ok)
	}
}

func TestBlocklistEmpty(t *testing.T) {
	b := NewBlocklist(nil)
	if b.Len() != 0 {
		t.Fatalf("len = %d, want 0", b.Len())
	}
	if _, ok := b.Match("example.com."); ok {
		t.Fatal("empty blocklist must not match")
	}
}

func TestBlocklistEqual(t *testing.T) {
	a := NewBlocklist([]string{"a.com", "b.com"})
	b := NewBlocklist([]string{"b.com", "a.com", "a.com"})
	if !a.Equal(b) {
		t.Fatal("blocklists must be equal")
	}
	if a.Equal(NewBlocklist([]string{"a.com"})) {
		t.Fatal("different blocklists must not be equal")
	}
	if !NewBlocklist(nil).Equal(nil) {
		t.Fatal("two empty blocklists must be equal")
	}
}
