package stats

import (
	"fmt"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"youtube.com.":              "youtube.com",
		"r3.sn-xyz.googlevideo.com": "r3.sn-xyz.googlevideo.com",
		"a.b.c.example.co.uk.":      "a.b.c.example.co.uk",
		"localhost":                 "localhost",
		"example.com":               "example.com",
		"":                          "",
		"  Example.COM.  ":          "example.com",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func info(rcode string) *AnswerInfo {
	return &AnswerInfo{
		Rcode:   rcode,
		Qtype:   "A",
		Records: []string{"example.com. 60 IN A 1.2.3.4"},
	}
}

func TestRecordCounters(t *testing.T) {
	s := newWithClock(0, func() time.Time { return time.Unix(1000, 0) })
	s.Record("a.example.com.", "g1", true, info("NOERROR"))
	s.Record("a.example.com.", "g1", true, info("NOERROR"))
	s.Record("b.example.com.", "g1", false, info("NOERROR"))
	s.Record("x.other.net.", "g2", false, info("SERVFAIL"))

	snap := s.Snapshot()
	if snap.Total != 4 {
		t.Errorf("Total = %d, want 4", snap.Total)
	}
	if snap.CacheHits != 2 || snap.CacheMisses != 2 {
		t.Errorf("hits/misses = %d/%d, want 2/2", snap.CacheHits, snap.CacheMisses)
	}
	if snap.Servfail != 1 {
		t.Errorf("Servfail = %d, want 1", snap.Servfail)
	}
	if snap.Groups["g1"] != 3 || snap.Groups["g2"] != 1 {
		t.Errorf("groups = %v", snap.Groups)
	}
	// Полные имена: три уникальных домена, без сведения к example.com.
	if snap.Domains != 3 {
		t.Errorf("Domains = %d, want 3", snap.Domains)
	}
	page := s.DomainsPage(1, 0, "")
	if len(page.Items) != 3 || page.Items[0].Domain != "a.example.com" || page.Items[0].Total != 2 {
		t.Fatalf("items = %+v", page.Items)
	}
}

func TestUpstreamsPage(t *testing.T) {
	s := newWithClock(0, func() time.Time { return time.Unix(1000, 0) })
	s.Record("a.test.", "g1", false, info("NOERROR"))
	s.Record("b.test.", "g1", true, info("NOERROR"))
	s.Record("c.test.", "g2", false, info("NOERROR"))
	s.RecordUpstream("g1", "8.8.8.8:53", false, 10*time.Millisecond)
	s.RecordUpstream("g1", "1.1.1.1:53", true, 200*time.Millisecond)
	s.RecordUpstream("g1", "1.1.1.1:53", false, 20*time.Millisecond)
	s.RecordUpstream("g2", "9.9.9.9:53", false, 5*time.Millisecond)
	s.RecordUpstream("g2", "9.9.9.9:53", false, 7*time.Millisecond)

	got := s.UpstreamsPage()
	if len(got) != 2 {
		t.Fatalf("groups = %+v", got)
	}
	// g1: 2 запроса (1 хит), g2: 1 запрос.
	if got[0].Name != "g1" || got[0].Total != 2 || got[0].CacheHits != 1 || got[0].Failed != 1 {
		t.Fatalf("g1 = %+v", got[0])
	}
	if len(got[0].Upstreams) != 2 || got[0].Upstreams[0].Upstream != "1.1.1.1:53" ||
		got[0].Upstreams[0].Ok != 1 || got[0].Upstreams[0].Failed != 1 {
		t.Fatalf("g1 upstreams = %+v", got[0].Upstreams)
	}
	// p90 учитывает и неудачные попытки: из {200, 20} мс p90 = 200.
	if p := got[0].Upstreams[0].P90Ms; p != 200 {
		t.Fatalf("1.1.1.1 p90 = %v, want 200", p)
	}
	if got[1].Name != "g2" || len(got[1].Upstreams) != 1 || got[1].Upstreams[0].Ok != 2 {
		t.Fatalf("g2 = %+v", got[1])
	}
	if p := got[1].Upstreams[0].P90Ms; p != 7 {
		t.Fatalf("9.9.9.9 p90 = %v, want 7", p)
	}
}

func TestUpstreamP90(t *testing.T) {
	s := newWithClock(0, func() time.Time { return time.Unix(0, 0) })
	for i := 1; i <= 10; i++ {
		s.RecordUpstream("g", "u", false, time.Duration(i)*time.Millisecond)
	}
	p90 := func() float64 { return s.UpstreamsPage()[0].Upstreams[0].P90Ms }
	// nearest-rank: ceil(0.9*10) = 9-й элемент отсортированного ряда.
	if got := p90(); got != 9 {
		t.Fatalf("p90 = %v, want 9", got)
	}

	// Неудачная попытка с большой задержкой тоже входит в выборку:
	// 11 значений, ceil(0.9*11) = 10 -> 10-й элемент = 10 мс.
	s.RecordUpstream("g", "u", true, 500*time.Millisecond)
	if got := p90(); got != 10 {
		t.Fatalf("p90 after failure = %v, want 10", got)
	}

	// Кольцевой буфер хранит только последние latencySamples значений:
	// после 300 записей остаются 45..300, p90 = 275.
	s2 := newWithClock(0, func() time.Time { return time.Unix(0, 0) })
	for i := 1; i <= 300; i++ {
		s2.RecordUpstream("g", "u", false, time.Duration(i)*time.Millisecond)
	}
	got := s2.UpstreamsPage()[0].Upstreams[0]
	if got.P90Ms != 275 {
		t.Fatalf("ring p90 = %v, want 275", got.P90Ms)
	}
}

func TestPeakRPMRollover(t *testing.T) {
	now := time.Unix(0, 0)
	s := newWithClock(0, func() time.Time { return now })
	top := func() DomainStat { return s.DomainsPage(1, 0, "").Items[0] }

	// Минута 0: 3 запроса.
	for i := 0; i < 3; i++ {
		s.Record("a.example.com.", "g", false, nil)
	}
	if got := top(); got.Current != 3 || got.PeakRPM != 0 {
		t.Fatalf("minute 0: %+v", got)
	}

	// Минута 1: 1 запрос -> rollover, peak = 3.
	now = time.Unix(60, 0)
	s.Record("a.example.com.", "g", false, nil)
	if got := top(); got.Current != 1 || got.PeakRPM != 3 {
		t.Fatalf("minute 1: %+v", got)
	}

	// Минута 2: 5 запросов -> peak остаётся 3, current 5.
	now = time.Unix(120, 0)
	for i := 0; i < 5; i++ {
		s.Record("a.example.com.", "g", false, nil)
	}
	if got := top(); got.Current != 5 || got.PeakRPM != 3 {
		t.Fatalf("minute 2: %+v", got)
	}

	// Минута 3: без запросов -> финализация минуты 2, peak = 5.
	now = time.Unix(180, 0)
	if got := top(); got.Current != 0 || got.PeakRPM != 5 {
		t.Fatalf("minute 3: %+v", got)
	}
	if got := top(); got.PeakRPM != 5 {
		t.Fatalf("minute 3 repeat: %+v", got)
	}
}

func TestDomainsPageSortPaginationAndClamp(t *testing.T) {
	s := newWithClock(0, func() time.Time { return time.Unix(0, 0) })
	for i := 0; i < 120; i++ {
		name := fmt.Sprintf("host%03d.test", i)
		for j := 0; j <= i; j++ {
			s.Record(name, "g", false, nil)
		}
	}

	first := s.DomainsPage(1, 50, "")
	if first.Total != 120 || first.Pages != 3 || first.Page != 1 || first.Size != 50 {
		t.Fatalf("page meta = %+v", first)
	}
	if len(first.Items) != 50 || first.Items[0].Domain != "host119.test" || first.Items[0].Total != 120 {
		t.Fatalf("first item = %+v", first.Items[0])
	}

	last := s.DomainsPage(3, 50, "")
	if last.Page != 3 || len(last.Items) != 20 {
		t.Fatalf("last page = %+v", last)
	}
	if last.Items[len(last.Items)-1].Domain != "host000.test" {
		t.Fatalf("last item = %+v", last.Items[len(last.Items)-1])
	}

	// Номер страницы за пределами — прижимается к последней.
	clamped := s.DomainsPage(99, 50, "")
	if clamped.Page != 3 || len(clamped.Items) != 20 {
		t.Fatalf("clamped = %+v", clamped)
	}

	// Некорректные page/size нормализуются.
	norm := s.DomainsPage(-1, -5, "")
	if norm.Page != 1 || norm.Size != DefaultPageSize || len(norm.Items) != 50 {
		t.Fatalf("normalized = %+v", norm)
	}
	big := s.DomainsPage(1, 10000, "")
	if big.Size != MaxPageSize || big.Pages != 1 || len(big.Items) != 120 {
		t.Fatalf("big size = %+v", big)
	}
}

func TestDomainsPageSearch(t *testing.T) {
	s := newWithClock(0, func() time.Time { return time.Unix(0, 0) })
	for i := 0; i < 30; i++ {
		s.Record(fmt.Sprintf("host%02d.test", i), "g", false, nil)
	}

	// Подстрока без учёта регистра: host1 -> host10..host19.
	got := s.DomainsPage(1, 0, "HOST1")
	if got.Total != 10 || len(got.Items) != 10 {
		t.Fatalf("search total = %d, items = %d, want 10", got.Total, len(got.Items))
	}
	for _, it := range got.Items {
		if it.Domain[:4] != "host" || it.Domain[4] != '1' {
			t.Fatalf("unexpected item %q", it.Domain)
		}
	}

	// Точное имя также находится как подстрока.
	exact := s.DomainsPage(1, 0, "host07.test")
	if exact.Total != 1 || exact.Items[0].Domain != "host07.test" {
		t.Fatalf("exact = %+v", exact)
	}

	empty := s.DomainsPage(1, 0, "missing")
	if empty.Total != 0 || empty.Pages != 0 || len(empty.Items) != 0 {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestMaxDomainsLimit(t *testing.T) {
	s := newWithClock(2, func() time.Time { return time.Unix(0, 0) })
	s.Record("a.test.", "g", false, nil)
	s.Record("b.test.", "g", false, nil)
	s.Record("c.test.", "g", false, nil) // сверх лимита — не учитывается
	s.Record("a.test.", "g", false, nil) // существующий домен учитывается

	snap := s.Snapshot()
	if snap.Total != 4 {
		t.Fatalf("Total = %d, want 4", snap.Total)
	}
	if snap.Domains != 2 {
		t.Fatalf("Domains = %d, want 2", snap.Domains)
	}
	if _, ok := s.DomainInfo("c.test"); ok {
		t.Fatal("c.test must not be tracked over the limit")
	}
	page := s.DomainsPage(1, 0, "")
	if len(page.Items) != 2 || page.Items[0].Domain != "a.test" || page.Items[0].Total != 2 {
		t.Fatalf("items = %+v", page.Items)
	}

	// Увеличение лимита снова разрешает новые домены.
	s.SetMaxDomains(3)
	s.Record("c.test.", "g", false, nil)
	if _, ok := s.DomainInfo("c.test"); !ok {
		t.Fatal("c.test must be tracked after raising the limit")
	}
}

func TestServfailPage(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newWithClock(0, func() time.Time { return now })

	s.Record("a.test.", "g1", false, info("NOERROR"))
	now = time.Unix(1010, 0)
	s.Record("a.test.", "g1", false, &AnswerInfo{
		Rcode:    "SERVFAIL",
		Qtype:    "A",
		Error:    "timeout",
		Upstream: "1.1.1.1:53",
	})
	now = time.Unix(1020, 0)
	s.Record("a.test.", "g1", false, &AnswerInfo{
		Rcode: "SERVFAIL",
		Qtype: "AAAA",
		Error: "boom",
	})
	now = time.Unix(1030, 0)
	s.Record("b.test.", "g2", true, &AnswerInfo{
		Rcode:     "SERVFAIL",
		Qtype:     "A",
		Upstream:  "8.8.8.8:53",
		FromCache: true,
	})
	s.Record("c.test.", "g1", false, info("NOERROR"))

	got := s.ServfailPage(1, 0)
	if got.Total != 3 || got.DomainsTotal != 2 {
		t.Fatalf("total/domains_total = %d/%d, want 3/2", got.Total, got.DomainsTotal)
	}
	if len(got.Domains) != 2 ||
		got.Domains[0].Domain != "a.test" || got.Domains[0].Count != 2 ||
		got.Domains[1].Domain != "b.test" || got.Domains[1].Count != 1 {
		t.Fatalf("domains = %+v", got.Domains)
	}
	if got.Domains[0].Last == nil || got.Domains[0].Last.Error != "boom" ||
		got.Domains[0].Last.Qtype != "AAAA" || !got.Domains[0].Last.At.Equal(time.Unix(1020, 0)) {
		t.Fatalf("a.test last = %+v", got.Domains[0].Last)
	}
	if got.Domains[1].Last == nil || !got.Domains[1].Last.FromCache ||
		got.Domains[1].Last.Upstream != "8.8.8.8:53" {
		t.Fatalf("b.test last = %+v", got.Domains[1].Last)
	}

	// Журнал событий: новые сверху, с группой и текстом ошибки.
	if len(got.Recent) != 3 {
		t.Fatalf("recent = %+v", got.Recent)
	}
	if got.Recent[0].Domain != "b.test" || got.Recent[0].Group != "g2" || !got.Recent[0].FromCache {
		t.Fatalf("recent[0] = %+v", got.Recent[0])
	}
	if got.Recent[2].Domain != "a.test" || got.Recent[2].Error != "timeout" ||
		got.Recent[2].Upstream != "1.1.1.1:53" || !got.Recent[2].At.Equal(time.Unix(1010, 0)) {
		t.Fatalf("recent[2] = %+v", got.Recent[2])
	}
}

func TestServfailPagePaginationAndRing(t *testing.T) {
	now := time.Unix(0, 0)
	s := newWithClock(0, func() time.Time { return now })

	// 120 доменов, у домена i ровно i+1 SERVFAIL.
	for i := 0; i < 120; i++ {
		name := fmt.Sprintf("host%03d.test", i)
		for j := 0; j <= i; j++ {
			s.Record(name, "g", false, info("SERVFAIL"))
		}
	}

	first := s.ServfailPage(1, 50)
	if first.Total != 7260 || first.DomainsTotal != 120 || first.Pages != 3 || first.Page != 1 {
		t.Fatalf("page meta = %+v", first)
	}
	if len(first.Domains) != 50 || first.Domains[0].Domain != "host119.test" || first.Domains[0].Count != 120 {
		t.Fatalf("first item = %+v", first.Domains[0])
	}

	clamped := s.ServfailPage(99, 50)
	if clamped.Page != 3 || len(clamped.Domains) != 20 {
		t.Fatalf("clamped = %+v", clamped)
	}

	// Кольцевой буфер ограничен servfailEventsCap, новые события сверху.
	s2 := newWithClock(0, func() time.Time { return now })
	for i := 0; i < 150; i++ {
		s2.Record(fmt.Sprintf("e%03d.test", i), "g", false, info("SERVFAIL"))
	}
	recent := s2.ServfailPage(1, 50).Recent
	if len(recent) != servfailEventsCap {
		t.Fatalf("recent len = %d, want %d", len(recent), servfailEventsCap)
	}
	if recent[0].Domain != "e149.test" {
		t.Fatalf("newest event = %+v", recent[0])
	}
	if recent[len(recent)-1].Domain != "e050.test" {
		t.Fatalf("oldest kept event = %+v", recent[len(recent)-1])
	}
}

func TestServfailBeyondDomainLimit(t *testing.T) {
	s := newWithClock(1, func() time.Time { return time.Unix(0, 0) })
	s.Record("a.test.", "g", false, info("SERVFAIL"))
	s.Record("b.test.", "g", false, info("SERVFAIL"))

	snap := s.Snapshot()
	if snap.Servfail != 2 || snap.Domains != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	got := s.ServfailPage(1, 0)
	if got.Total != 2 || got.DomainsTotal != 1 {
		t.Fatalf("page = %+v", got)
	}
	if len(got.Recent) != 2 || got.Recent[0].Domain != "b.test" {
		t.Fatalf("recent = %+v", got.Recent)
	}
}

func TestDomainInfoLastAnswers(t *testing.T) {
	now := time.Unix(100, 0)
	s := newWithClock(0, func() time.Time { return now })
	s.Record("api.Example.com.", "g", false, &AnswerInfo{
		Rcode:    "NOERROR",
		Qtype:    "A",
		Records:  []string{"api.example.com. 60 IN A 1.2.3.4"},
		Upstream: "1.1.1.1:53",
	})
	now = time.Unix(110, 0)
	s.Record("api.example.com.", "g", true, &AnswerInfo{
		Rcode:      "NXDOMAIN",
		Qtype:      "AAAA",
		Authority:  []string{"example.com. 60 IN SOA ns. hostmaster. 1 2 3 4 60"},
		FromCache:  true,
		DurationMs: 34.5,
	})
	// Новый A-ответ обновляет только запись своего типа, не затирая AAAA.
	now = time.Unix(120, 0)
	s.Record("api.example.com.", "g", false, &AnswerInfo{
		Rcode:   "NOERROR",
		Qtype:   "A",
		Records: []string{"api.example.com. 30 IN A 5.6.7.8"},
	})

	di, ok := s.DomainInfo("API.EXAMPLE.COM")
	if !ok {
		t.Fatal("domain not found")
	}
	if di.Domain != "api.example.com" || di.Total != 3 {
		t.Fatalf("info = %+v", di)
	}
	// Свежие сверху: A (120), затем AAAA (110).
	if len(di.Answers) != 2 {
		t.Fatalf("answers = %+v", di.Answers)
	}
	if a := di.Answers[0]; a.Qtype != "A" || a.Rcode != "NOERROR" || len(a.Records) != 1 ||
		a.Records[0] != "api.example.com. 30 IN A 5.6.7.8" || !a.At.Equal(time.Unix(120, 0)) {
		t.Fatalf("answers[0] = %+v", a)
	}
	if a := di.Answers[1]; a.Qtype != "AAAA" || a.Rcode != "NXDOMAIN" || !a.FromCache ||
		a.DurationMs != 34.5 || len(a.Authority) != 1 || len(a.Records) != 0 ||
		!a.At.Equal(time.Unix(110, 0)) {
		t.Fatalf("answers[1] = %+v", a)
	}

	if _, ok := s.DomainInfo("missing.test"); ok {
		t.Fatal("missing domain must not be found")
	}
	if _, ok := s.DomainInfo(""); ok {
		t.Fatal("empty domain must not be found")
	}
}

// TestAnswersPerDomainLimit проверяет, что число последних ответов на домен
// ограничено: при переполнении вытесняется самый старый тип.
func TestAnswersPerDomainLimit(t *testing.T) {
	now := time.Unix(0, 0)
	s := newWithClock(0, func() time.Time { return now })
	for i := 0; i < maxAnswersPerDomain+2; i++ {
		now = time.Unix(int64(i), 0)
		s.Record("a.test.", "g", false, &AnswerInfo{
			Rcode: "NOERROR",
			Qtype: fmt.Sprintf("T%d", i),
		})
	}
	di, ok := s.DomainInfo("a.test")
	if !ok {
		t.Fatal("domain not found")
	}
	if len(di.Answers) != maxAnswersPerDomain {
		t.Fatalf("answers = %d, want %d", len(di.Answers), maxAnswersPerDomain)
	}
	// Самые старые типы вытеснены.
	for _, a := range di.Answers {
		if a.Qtype == "T0" || a.Qtype == "T1" {
			t.Fatalf("oldest answer %q must be evicted", a.Qtype)
		}
	}
}
