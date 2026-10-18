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
	page := s.DomainsPage(1, 0, "", false, false, "", nil)
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
	top := func() DomainStat { return s.DomainsPage(1, 0, "", false, false, "", nil).Items[0] }

	// Минута 0: 3 запроса.
	for i := 0; i < 3; i++ {
		s.Record("a.example.com.", "g", false, nil)
	}
	if got := top(); got.PeakRPM != 0 {
		t.Fatalf("minute 0: %+v", got)
	}

	// Минута 1: 1 запрос -> rollover, peak = 3.
	now = time.Unix(60, 0)
	s.Record("a.example.com.", "g", false, nil)
	if got := top(); got.PeakRPM != 3 {
		t.Fatalf("minute 1: %+v", got)
	}

	// Минута 2: 5 запросов -> peak остаётся 3.
	now = time.Unix(120, 0)
	for i := 0; i < 5; i++ {
		s.Record("a.example.com.", "g", false, nil)
	}
	if got := top(); got.PeakRPM != 3 {
		t.Fatalf("minute 2: %+v", got)
	}

	// Минута 3: без запросов -> финализация минуты 2, peak = 5.
	now = time.Unix(180, 0)
	if got := top(); got.PeakRPM != 5 {
		t.Fatalf("minute 3: %+v", got)
	}
	if got := top(); got.PeakRPM != 5 {
		t.Fatalf("minute 3 repeat: %+v", got)
	}
}

func TestDomainCacheHits(t *testing.T) {
	now := time.Unix(0, 0)
	s := newWithClock(0, func() time.Time { return now })
	top := func() DomainStat { return s.DomainsPage(1, 0, "", false, false, "", nil).Items[0] }

	s.Record("a.example.com.", "g", true, nil)
	s.Record("a.example.com.", "g", false, nil)
	s.Record("a.example.com.", "g", true, nil)

	got := top()
	if got.CacheHits != 2 {
		t.Fatalf("cache hits = %d, want 2 (%+v)", got.CacheHits, got)
	}
	if got.Total != 3 {
		t.Fatalf("total = %d, want 3 (%+v)", got.Total, got)
	}

	// Хиты накапливаются между минутами и не сбрасываются при rollover.
	now = time.Unix(60, 0)
	s.Record("a.example.com.", "g", true, nil)
	if got := top(); got.CacheHits != 3 {
		t.Fatalf("cache hits after rollover = %d, want 3 (%+v)", got.CacheHits, got)
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

	first := s.DomainsPage(1, 50, "", false, false, "", nil)
	if first.Total != 120 || first.Pages != 3 || first.Page != 1 || first.Size != 50 {
		t.Fatalf("page meta = %+v", first)
	}
	if len(first.Items) != 50 || first.Items[0].Domain != "host119.test" || first.Items[0].Total != 120 {
		t.Fatalf("first item = %+v", first.Items[0])
	}

	last := s.DomainsPage(3, 50, "", false, false, "", nil)
	if last.Page != 3 || len(last.Items) != 20 {
		t.Fatalf("last page = %+v", last)
	}
	if last.Items[len(last.Items)-1].Domain != "host000.test" {
		t.Fatalf("last item = %+v", last.Items[len(last.Items)-1])
	}

	// Номер страницы за пределами — прижимается к последней.
	clamped := s.DomainsPage(99, 50, "", false, false, "", nil)
	if clamped.Page != 3 || len(clamped.Items) != 20 {
		t.Fatalf("clamped = %+v", clamped)
	}

	// Некорректные page/size нормализуются.
	norm := s.DomainsPage(-1, -5, "", false, false, "", nil)
	if norm.Page != 1 || norm.Size != DefaultPageSize || len(norm.Items) != 50 {
		t.Fatalf("normalized = %+v", norm)
	}
	big := s.DomainsPage(1, 10000, "", false, false, "", nil)
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
	got := s.DomainsPage(1, 0, "HOST1", false, false, "", nil)
	if got.Total != 10 || len(got.Items) != 10 {
		t.Fatalf("search total = %d, items = %d, want 10", got.Total, len(got.Items))
	}
	for _, it := range got.Items {
		if it.Domain[:4] != "host" || it.Domain[4] != '1' {
			t.Fatalf("unexpected item %q", it.Domain)
		}
	}

	// Точное имя также находится как подстрока.
	exact := s.DomainsPage(1, 0, "host07.test", false, false, "", nil)
	if exact.Total != 1 || exact.Items[0].Domain != "host07.test" {
		t.Fatalf("exact = %+v", exact)
	}

	empty := s.DomainsPage(1, 0, "missing", false, false, "", nil)
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
	page := s.DomainsPage(1, 0, "", false, false, "", nil)
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

func TestServfailBeyondDomainLimit(t *testing.T) {
	s := newWithClock(1, func() time.Time { return time.Unix(0, 0) })
	s.Record("a.test.", "g", false, info("SERVFAIL"))
	s.Record("b.test.", "g", false, info("SERVFAIL"))

	// Журнала событий больше нет, но общий счётчик не зависит от лимита
	// доменов и учитывает оба SERVFAIL.
	snap := s.Snapshot()
	if snap.Servfail != 2 || snap.Domains != 1 {
		t.Fatalf("snapshot = %+v", snap)
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
	// NXDOMAIN не сохраняется и не затирает успешный ответ.
	now = time.Unix(110, 0)
	s.Record("api.example.com.", "g", true, &AnswerInfo{
		Rcode:      "NXDOMAIN",
		Qtype:      "AAAA",
		Authority:  []string{"example.com. 60 IN SOA ns. hostmaster. 1 2 3 4 60"},
		FromCache:  true,
		DurationMs: 34.5,
	})
	// Успешный AAAA-ответ добавляет записи к сводке; общий CNAME
	// дедуплицируется.
	now = time.Unix(120, 0)
	s.Record("api.example.com.", "g", false, &AnswerInfo{
		Rcode:    "NOERROR",
		Qtype:    "AAAA",
		Records:  []string{"api.example.com. 30 IN AAAA 2001:db8::1", "api.example.com. 30 IN CNAME alias.example.com."},
		Upstream: "8.8.8.8:53",
	})
	// Повторный A-ответ заменяет прежние A-записи своего типа.
	now = time.Unix(130, 0)
	s.Record("api.example.com.", "g", false, &AnswerInfo{
		Rcode:    "NOERROR",
		Qtype:    "A",
		Records:  []string{"api.example.com. 30 IN A 5.6.7.8", "api.example.com. 30 IN CNAME alias.example.com."},
		Upstream: "9.9.9.9:53",
	})
	// SERVFAIL тоже не попадает в сводку и не затирает записи.
	now = time.Unix(140, 0)
	s.Record("api.example.com.", "g", false, &AnswerInfo{
		Rcode:    "SERVFAIL",
		Qtype:    "A",
		Error:    "timeout",
		Upstream: "1.1.1.1:53",
	})

	di, ok := s.DomainInfo("API.EXAMPLE.COM")
	if !ok {
		t.Fatal("domain not found")
	}
	if di.Domain != "api.example.com" || di.Total != 5 {
		t.Fatalf("info = %+v", di)
	}
	if di.Answer == nil {
		t.Fatal("answer must be present")
	}
	a := di.Answer
	want := []string{
		"api.example.com. 30 IN A 5.6.7.8",
		"api.example.com. 30 IN AAAA 2001:db8::1",
		"api.example.com. 30 IN CNAME alias.example.com.",
	}
	if a.Rcode != "NOERROR" || a.Qtype != "" || len(a.Records) != len(want) {
		t.Fatalf("answer = %+v", a)
	}
	for i, rec := range want {
		if a.Records[i] != rec {
			t.Fatalf("records[%d] = %q, want %q", i, a.Records[i], rec)
		}
	}
	// Qtype — сводный ответ; атрибуты — от самого свежего успешного ответа.
	if a.Upstream != "9.9.9.9:53" || a.FromCache || !a.At.Equal(time.Unix(130, 0)) {
		t.Fatalf("answer attributes = %+v", a)
	}
	if len(a.Authority) != 0 {
		t.Fatalf("authority = %+v", a.Authority)
	}
	// Last — самый свежий выданный ответ (SERVFAIL), именно по нему строится
	// Status в таблице доменов.
	if di.Last == nil || di.Last.Rcode != "SERVFAIL" || di.Last.Error != "timeout" ||
		!di.Last.At.Equal(time.Unix(140, 0)) {
		t.Fatalf("last = %+v", di.Last)
	}

	// Домен без успешных ответов: статистика есть, сводки нет.
	now = time.Unix(150, 0)
	s.Record("nx.example.com.", "g", false, &AnswerInfo{Rcode: "NXDOMAIN", Qtype: "A"})
	di, ok = s.DomainInfo("nx.example.com")
	if !ok || di.Answer != nil {
		t.Fatalf("info = %+v", di)
	}
	if di.Last == nil || di.Last.Rcode != "NXDOMAIN" {
		t.Fatalf("last = %+v", di.Last)
	}

	if _, ok := s.DomainInfo("missing.test"); ok {
		t.Fatal("missing domain must not be found")
	}
	if _, ok := s.DomainInfo(""); ok {
		t.Fatal("empty domain must not be found")
	}
}

// TestDomainInfoNegative проверяет учёт негативных ответов: NODATA/NXDOMAIN
// хранятся по qtype с цепочкой попыток, не затирают успешные записи, а свежий
// успешный ответ снимает негативный того же типа.
// TestDomainInfoLastCopiesAttempts проверяет, что Last хранит копию ответа:
// изменение исходного среза попыток не влияет на статистику.
func TestDomainInfoLastCopiesAttempts(t *testing.T) {
	s := New(0)
	info := &AnswerInfo{
		Rcode:    "SERVFAIL",
		Qtype:    "A",
		Attempts: []AttemptInfo{{Upstream: "x", Status: "ERROR"}},
	}
	s.Record("copy.test.", "g", false, info)
	info.Attempts[0].Upstream = "mutated"
	di, ok := s.DomainInfo("copy.test")
	if !ok || di.Last == nil || len(di.Last.Attempts) != 1 || di.Last.Attempts[0].Upstream != "x" {
		t.Fatalf("last attempts must be copied: %+v", di.Last)
	}
}

func TestDomainInfoNegative(t *testing.T) {
	now := time.Unix(200, 0)
	s := newWithClock(0, func() time.Time { return now })
	s.Record("api.example.com.", "g", false, &AnswerInfo{
		Rcode:   "NOERROR",
		Qtype:   "A",
		Records: []string{"api.example.com. 60 IN A 1.2.3.4"},
	})
	// NODATA по AAAA: основной ответил NODATA, резерв проверял — тоже NODATA.
	now = time.Unix(210, 0)
	s.Record("api.example.com.", "g", false, &AnswerInfo{
		Rcode:     "NOERROR",
		Qtype:     "AAAA",
		Authority: []string{"example.com. 60 IN SOA ns. hostmaster. 1 2 3 4 60"},
		Attempts: []AttemptInfo{
			{Upstream: "udp://192.168.1.1:53", Status: "NODATA", DurationMs: 4},
			{Upstream: "doh+https://9.9.9.9/dns-query", Status: "NODATA", DurationMs: 40},
		},
	})
	// NXDOMAIN по HTTPS (из кэша).
	now = time.Unix(220, 0)
	s.Record("api.example.com.", "g", true, &AnswerInfo{
		Rcode:     "NXDOMAIN",
		Qtype:     "HTTPS",
		FromCache: true,
	})

	di, ok := s.DomainInfo("api.example.com")
	if !ok {
		t.Fatal("domain not found")
	}
	// Негативные ответы не затирают успешную сводку.
	if di.Answer == nil || di.Answer.Rcode != "NOERROR" || len(di.Answer.Records) != 1 {
		t.Fatalf("answer = %+v", di.Answer)
	}
	if len(di.Negative) != 2 {
		t.Fatalf("negative = %+v", di.Negative)
	}
	// Свежие сверху.
	if di.Negative[0].Qtype != "HTTPS" || di.Negative[0].Rcode != "NXDOMAIN" || !di.Negative[0].FromCache {
		t.Fatalf("negative[0] = %+v", di.Negative[0])
	}
	aaaa := di.Negative[1]
	if aaaa.Qtype != "AAAA" || aaaa.Rcode != "NOERROR" || len(aaaa.Authority) != 1 {
		t.Fatalf("negative[1] = %+v", aaaa)
	}
	if len(aaaa.Attempts) != 2 || aaaa.Attempts[0].Status != "NODATA" ||
		aaaa.Attempts[1].Upstream != "doh+https://9.9.9.9/dns-query" {
		t.Fatalf("attempts = %+v", aaaa.Attempts)
	}

	// Глубокая копия: правка выданной сводки не меняет хранилище.
	di.Negative[1].Attempts[0].Upstream = "mutated"
	if again, _ := s.DomainInfo("api.example.com"); again.Negative[1].Attempts[0].Upstream == "mutated" {
		t.Fatal("attempts must be cloned")
	}

	// Свежий успешный AAAA-ответ снимает негативный по тому же типу.
	now = time.Unix(230, 0)
	s.Record("api.example.com.", "g", false, &AnswerInfo{
		Rcode:   "NOERROR",
		Qtype:   "AAAA",
		Records: []string{"api.example.com. 30 IN AAAA 2001:db8::1"},
	})
	di, _ = s.DomainInfo("api.example.com")
	if len(di.Negative) != 1 || di.Negative[0].Qtype != "HTTPS" {
		t.Fatalf("negative after success = %+v", di.Negative)
	}
}

// TestAnswersPerDomainLimit проверяет, что число последних успешных ответов
// на домен ограничено: при переполнении вытесняется самый старый тип.
func TestAnswersPerDomainLimit(t *testing.T) {
	now := time.Unix(0, 0)
	s := newWithClock(0, func() time.Time { return now })
	for i := 0; i < maxAnswersPerDomain+2; i++ {
		now = time.Unix(int64(i), 0)
		s.Record("a.test.", "g", false, &AnswerInfo{
			Rcode:   "NOERROR",
			Qtype:   fmt.Sprintf("T%d", i),
			Records: []string{fmt.Sprintf("a.test. 60 IN TXT rec%d", i)},
		})
	}
	di, ok := s.DomainInfo("a.test")
	if !ok {
		t.Fatal("domain not found")
	}
	if di.Answer == nil {
		t.Fatal("answer must be present")
	}
	if len(di.Answer.Records) != maxAnswersPerDomain {
		t.Fatalf("records = %d, want %d", len(di.Answer.Records), maxAnswersPerDomain)
	}
	// Самые старые типы вытеснены.
	for _, rec := range di.Answer.Records {
		if rec == "a.test. 60 IN TXT rec0" || rec == "a.test. 60 IN TXT rec1" {
			t.Fatalf("oldest record %q must be evicted", rec)
		}
	}
}

// TestRecordBlocked проверяет, что заблокированный ответ не считается
// попаданием/промахом кэша, увеличивает счётчик Blocked и сохраняется в сводке
// домена как ответ блоклиста.
func TestRecordBlocked(t *testing.T) {
	s := New(0)
	s.Record("ads.example.com.", "", false, &AnswerInfo{
		Rcode:     "NOERROR",
		Qtype:     "A",
		Blocked:   true,
		BlockedBy: "example.com",
	})

	snap := s.Snapshot()
	if snap.Blocked != 1 || snap.CacheHits != 0 || snap.CacheMisses != 0 || snap.Total != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	di, ok := s.DomainInfo("ads.example.com")
	if !ok || di.Answer == nil || !di.Answer.Blocked || di.Answer.BlockedBy != "example.com" {
		t.Fatalf("domain info = %+v", di)
	}
	if len(di.Negative) != 0 {
		t.Fatalf("blocked answer must not be negative: %+v", di.Negative)
	}
}

// TestDomainsPageBlockedFilter проверяет простановку флагов блокировки и фильтр
// «только заблокированные».
func TestDomainsPageBlockedFilter(t *testing.T) {
	s := New(0)
	s.Record("a.example.com.", "g", false, nil)
	s.Record("b.other.net.", "g", false, nil)
	isBlocked := func(name string) (string, bool) {
		if name == "a.example.com" {
			return "example.com", true
		}
		return "", false
	}

	all := s.DomainsPage(1, 0, "", false, false, "", isBlocked)
	if all.Total != 2 {
		t.Fatalf("total = %d, want 2", all.Total)
	}
	for _, item := range all.Items {
		want := item.Domain == "a.example.com"
		if item.Blocked != want {
			t.Fatalf("item %+v blocked = %v, want %v", item, item.Blocked, want)
		}
		if want && item.BlockedBy != "example.com" {
			t.Fatalf("blocked_by = %q", item.BlockedBy)
		}
	}

	only := s.DomainsPage(1, 0, "", true, false, "", isBlocked)
	if only.Total != 1 || len(only.Items) != 1 || only.Items[0].Domain != "a.example.com" {
		t.Fatalf("filtered page = %+v", only)
	}
}

// TestDomainStatus проверяет колонку Status: состояние последнего ответа
// домена (успешный, негативный, SERVFAIL, блокировка) и приоритет блоклиста.
func TestDomainStatus(t *testing.T) {
	now := time.Unix(0, 0)
	s := newWithClock(0, func() time.Time { return now })
	status := func(name string) string {
		page := s.DomainsPage(1, 0, name, false, false, "", nil)
		if len(page.Items) != 1 {
			t.Fatalf("status(%q): items = %+v", name, page.Items)
		}
		return page.Items[0].Status
	}
	answer := func(rcode string, records ...string) *AnswerInfo {
		return &AnswerInfo{Rcode: rcode, Qtype: "A", Records: records}
	}

	// Ответов не было — статус пуст.
	s.Record("none.test.", "g", false, nil)
	if got := status("none.test"); got != "" {
		t.Fatalf("no answer status = %q, want empty", got)
	}

	s.Record("ok.test.", "g", false, answer("NOERROR", "ok.test. 60 IN A 1.2.3.4"))
	if got := status("ok.test"); got != "ok" {
		t.Fatalf("ok status = %q", got)
	}

	s.Record("nx.test.", "g", false, answer("NXDOMAIN"))
	if got := status("nx.test"); got != "nxdomain" {
		t.Fatalf("nxdomain status = %q", got)
	}

	s.Record("nodata.test.", "g", false, answer("NOERROR"))
	if got := status("nodata.test"); got != "nodata" {
		t.Fatalf("nodata status = %q", got)
	}

	s.Record("fail.test.", "g", false, answer("SERVFAIL"))
	if got := status("fail.test"); got != "servfail" {
		t.Fatalf("servfail status = %q", got)
	}

	s.Record("blocked.test.", "g", false, &AnswerInfo{
		Rcode:     "NOERROR",
		Qtype:     "A",
		Blocked:   true,
		BlockedBy: "test",
		Records:   []string{"blocked.test. 60 IN A 0.0.0.0"},
	})
	if got := status("blocked.test"); got != "blocked" {
		t.Fatalf("blocked status = %q", got)
	}

	// Более свежий ответ перекрывает прежний: ok -> servfail -> ok.
	now = time.Unix(10, 0)
	s.Record("flip.test.", "g", false, answer("NOERROR", "flip.test. 60 IN A 1.1.1.1"))
	if got := status("flip.test"); got != "ok" {
		t.Fatalf("flip initial = %q", got)
	}
	now = time.Unix(20, 0)
	s.Record("flip.test.", "g", true, answer("SERVFAIL"))
	if got := status("flip.test"); got != "servfail" {
		t.Fatalf("flip after servfail = %q", got)
	}
	now = time.Unix(30, 0)
	s.Record("flip.test.", "g", false, answer("NOERROR", "flip.test. 60 IN A 2.2.2.2"))
	if got := status("flip.test"); got != "ok" {
		t.Fatalf("flip after recovery = %q", got)
	}

	// Статус строится строго по хронологически последнему ответу, даже если
	// это негативный ответ другого типа запроса.
	now = time.Unix(40, 0)
	s.Record("multi.test.", "g", false, &AnswerInfo{
		Rcode: "NOERROR", Qtype: "A", Records: []string{"multi.test. 60 IN A 1.1.1.1"},
	})
	now = time.Unix(50, 0)
	s.Record("multi.test.", "g", false, &AnswerInfo{Rcode: "NOERROR", Qtype: "AAAA"})
	if got := status("multi.test"); got != "nodata" {
		t.Fatalf("cross-qtype status = %q, want nodata", got)
	}
	now = time.Unix(60, 0)
	s.Record("multi.test.", "g", false, &AnswerInfo{
		Rcode: "NOERROR", Qtype: "A", Records: []string{"multi.test. 60 IN A 2.2.2.2"},
	})
	if got := status("multi.test"); got != "ok" {
		t.Fatalf("cross-qtype recovery = %q, want ok", got)
	}

	// Домен, заблокированный сейчас, получает статус blocked даже если
	// последний сохранённый ответ был успешным.
	isBlocked := func(name string) (string, bool) {
		if name == "ok.test" {
			return "ok.test", true
		}
		return "", false
	}
	page := s.DomainsPage(1, 0, "ok.test", false, false, "", isBlocked)
	if len(page.Items) != 1 || page.Items[0].Status != "blocked" {
		t.Fatalf("current blocked status = %+v", page.Items)
	}
}

// TestDomainsPageStatusFilter проверяет фильтр по статусу последнего ответа:
// status=bad оставляет только домены с плохим ответом, а текущая блокировка
// (статус blocked) исключает домен из выборки.
func TestDomainsPageStatusFilter(t *testing.T) {
	s := New(0)
	answer := func(rcode string, records ...string) *AnswerInfo {
		return &AnswerInfo{Rcode: rcode, Qtype: "A", Records: records}
	}
	s.Record("ok.test.", "g", false, answer("NOERROR", "ok.test. 60 IN A 1.2.3.4"))
	s.Record("bad.test.", "g", false, answer("REFUSED"))
	s.Record("fail.test.", "g", false, answer("SERVFAIL"))
	s.Record("blocked.test.", "g", false, answer("REFUSED"))
	isBlocked := func(name string) (string, bool) {
		if name == "blocked.test" {
			return "blocked.test", true
		}
		return "", false
	}

	bad := s.DomainsPage(1, 0, "", false, false, "bad", isBlocked)
	if bad.Total != 1 || len(bad.Items) != 1 || bad.Items[0].Domain != "bad.test" {
		t.Fatalf("status=bad page = %+v", bad)
	}

	ok := s.DomainsPage(1, 0, "", false, false, "ok", isBlocked)
	if ok.Total != 1 || len(ok.Items) != 1 || ok.Items[0].Domain != "ok.test" {
		t.Fatalf("status=ok page = %+v", ok)
	}

	blocked := s.DomainsPage(1, 0, "", false, false, "blocked", isBlocked)
	if blocked.Total != 1 || len(blocked.Items) != 1 || blocked.Items[0].Domain != "blocked.test" {
		t.Fatalf("status=blocked page = %+v", blocked)
	}

	// bad=1 оставляет домены с любым проблемным последним ответом
	// (bad/servfail/nxdomain/nodata), но не ok, не blocked и не пустые.
	badOnly := s.DomainsPage(1, 0, "", false, true, "", isBlocked)
	if badOnly.Total != 2 || len(badOnly.Items) != 2 ||
		badOnly.Items[0].Domain != "bad.test" || badOnly.Items[1].Domain != "fail.test" {
		t.Fatalf("bad=1 page = %+v", badOnly)
	}
}

// TestSnapshotBadCounter проверяет счётчик bad-ответов: в него попадают только
// ответы с ошибкой (не ok/servfail/nxdomain/nodata/blocked), включая ответы
// из кэша.
func TestSnapshotBadCounter(t *testing.T) {
	s := New(0)
	answer := func(rcode string, records ...string) *AnswerInfo {
		return &AnswerInfo{Rcode: rcode, Qtype: "A", Records: records}
	}
	s.Record("ok.test.", "g", false, answer("NOERROR", "ok.test. 60 IN A 1.2.3.4"))
	s.Record("nx.test.", "g", false, answer("NXDOMAIN"))
	s.Record("nodata.test.", "g", false, answer("NOERROR"))
	s.Record("fail.test.", "g", false, answer("SERVFAIL"))
	s.Record("blocked.test.", "g", false, &AnswerInfo{
		Rcode:   "NOERROR",
		Qtype:   "A",
		Blocked: true,
		Records: []string{"blocked.test. 60 IN A 0.0.0.0"},
	})
	if got := s.Snapshot().Bad; got != 0 {
		t.Fatalf("bad = %d, want 0", got)
	}
	if got := s.Snapshot().Servfail; got != 1 {
		t.Fatalf("servfail = %d, want 1", got)
	}
	if got := s.Snapshot().Nxdomain; got != 1 {
		t.Fatalf("nxdomain = %d, want 1", got)
	}
	if got := s.Snapshot().Nodata; got != 1 {
		t.Fatalf("nodata = %d, want 1", got)
	}

	// Ответы из кэша учитываются так же, как свежие.
	s.Record("nx.test.", "g", true, answer("NXDOMAIN"))
	s.Record("nodata.test.", "g", true, answer("NOERROR"))
	if got := s.Snapshot().Nxdomain; got != 2 {
		t.Fatalf("cached nxdomain = %d, want 2", got)
	}
	if got := s.Snapshot().Nodata; got != 2 {
		t.Fatalf("cached nodata = %d, want 2", got)
	}

	s.Record("refused.test.", "g", false, answer("REFUSED"))
	s.Record("refused.test.", "g", true, answer("REFUSED"))
	if got := s.Snapshot().Bad; got != 2 {
		t.Fatalf("bad = %d, want 2", got)
	}
	// Заблокированный ответ не должен учитываться как bad.
	if got := s.Snapshot().Blocked; got != 1 {
		t.Fatalf("blocked = %d, want 1", got)
	}
}
