package stats

import (
	"testing"
	"time"
)

func TestBaseDomain(t *testing.T) {
	cases := map[string]string{
		"youtube.com.":              "youtube.com",
		"r3.sn-xyz.googlevideo.com": "googlevideo.com",
		"a.b.c.example.co.uk.":      "co.uk",
		"localhost":                 "localhost",
		"example.com":               "example.com",
		"":                          "",
		"  Example.COM.  ":          "example.com",
	}
	for in, want := range cases {
		if got := baseDomain(in); got != want {
			t.Errorf("baseDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecordCounters(t *testing.T) {
	s := newWithClock(func() time.Time { return time.Unix(1000, 0) })
	s.Record("a.example.com.", "g1", true, false)
	s.Record("b.example.com.", "g1", false, false)
	s.Record("x.other.net.", "g2", false, true)

	snap := s.Snapshot(10)
	if snap.Total != 3 {
		t.Errorf("Total = %d, want 3", snap.Total)
	}
	if snap.CacheHits != 1 || snap.CacheMisses != 2 {
		t.Errorf("hits/misses = %d/%d, want 1/2", snap.CacheHits, snap.CacheMisses)
	}
	if snap.Servfail != 1 {
		t.Errorf("Servfail = %d, want 1", snap.Servfail)
	}
	if snap.Groups["g1"] != 2 || snap.Groups["g2"] != 1 {
		t.Errorf("groups = %v", snap.Groups)
	}
	if snap.Domains != 2 {
		t.Errorf("Domains = %d, want 2", snap.Domains)
	}
	// example.com должен быть первым: 2 запроса против 1.
	if len(snap.Top) != 2 || snap.Top[0].Domain != "example.com" || snap.Top[0].Total != 2 {
		t.Fatalf("Top = %+v", snap.Top)
	}
}

func TestPeakRPMRollover(t *testing.T) {
	now := time.Unix(0, 0)
	s := newWithClock(func() time.Time { return now })

	// Минута 0: 3 запроса.
	for i := 0; i < 3; i++ {
		s.Record("a.example.com.", "g", false, false)
	}
	if top := s.TopN(1); top[0].Current != 3 || top[0].PeakRPM != 0 {
		t.Fatalf("minute 0: %+v", top[0])
	}

	// Минута 1: 1 запрос -> rollover, peak = 3.
	now = time.Unix(60, 0)
	s.Record("a.example.com.", "g", false, false)
	if top := s.TopN(1); top[0].Current != 1 || top[0].PeakRPM != 3 {
		t.Fatalf("minute 1: %+v", top[0])
	}

	// Минута 2: 5 запросов -> peak остаётся 3, current 5.
	now = time.Unix(120, 0)
	for i := 0; i < 5; i++ {
		s.Record("a.example.com.", "g", false, false)
	}
	if top := s.TopN(1); top[0].Current != 5 || top[0].PeakRPM != 3 {
		t.Fatalf("minute 2: %+v", top[0])
	}

	// Минута 3: без запросов -> TopN финализирует минуту 2, peak = 5.
	now = time.Unix(180, 0)
	if top := s.TopN(1); top[0].Current != 0 || top[0].PeakRPM != 5 {
		t.Fatalf("minute 3: %+v", top[0])
	}
	if top := s.TopN(1); top[0].PeakRPM != 5 {
		t.Fatalf("minute 3 repeat: %+v", top[0])
	}
}

func TestTopNLimitAndSort(t *testing.T) {
	s := newWithClock(func() time.Time { return time.Unix(0, 0) })
	for i := 0; i < 5; i++ {
		s.Record("hot.example.com.", "g", false, false)
	}
	s.Record("warm.net.", "g", false, false)
	s.Record("cold.org.", "g", false, false)

	top := s.TopN(2)
	if len(top) != 2 {
		t.Fatalf("len = %d, want 2", len(top))
	}
	if top[0].Domain != "example.com" || top[1].Domain != "cold.org" {
		t.Fatalf("top = %+v", top)
	}
}
