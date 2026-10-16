package rules

import (
	"testing"

	"github.com/nskforward/dns-guard/internal/config"
)

func TestMatchLongestSuffix(t *testing.T) {
	m := New([]config.Rule{
		{Zone: "com", Group: "gcom"},
		{Zone: "example.com", Group: "gex"},
		{Zone: "ru", Group: "gru"},
	})

	cases := []struct {
		name  string
		group string
		ok    bool
	}{
		{"a.example.com.", "gex", true},
		{"example.com.", "gex", true},
		{"other.com.", "gcom", true},
		{"sub.other.com.", "gcom", true},
		{"yandex.ru.", "gru", true},
		{"example.net.", "", false},
		{"notcom.", "", false}, // не должно совпадать с "com"
	}
	for _, c := range cases {
		g, ok := m.Match(c.name)
		if ok != c.ok || g != c.group {
			t.Errorf("Match(%q) = (%q,%v), want (%q,%v)", c.name, g, ok, c.group, c.ok)
		}
	}
}

func TestMatchWildcardAndCase(t *testing.T) {
	m := New([]config.Rule{{Zone: "*.ru", Group: "gru"}})
	if g, ok := m.Match("EXAMPLE.RU."); !ok || g != "gru" {
		t.Fatalf("Match = (%q,%v)", g, ok)
	}
}

func TestMatchEmptyRules(t *testing.T) {
	m := New(nil)
	if _, ok := m.Match("example.com."); ok {
		t.Fatalf("empty matcher must not match")
	}
}
