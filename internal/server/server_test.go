package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/nskforward/dns-guard/internal/cache"
	"github.com/nskforward/dns-guard/internal/config"
	"github.com/nskforward/dns-guard/internal/rules"
	"github.com/nskforward/dns-guard/internal/stats"
	"github.com/nskforward/dns-guard/internal/upstream"
)

// fakeExchanger — подменная вышестоящая группа.
type fakeExchanger struct {
	resp     *dns.Msg
	err      error
	attempts []upstream.Attempt // если задано — возвращается как цепочка попыток
	last     *dns.Msg
	calls    int
}

func (f *fakeExchanger) Exchange(_ context.Context, m *dns.Msg) (*dns.Msg, string, []upstream.Attempt, error) {
	f.calls++
	f.last = m
	origin := "fake"
	if len(f.attempts) > 0 {
		origin = f.attempts[len(f.attempts)-1].Upstream
	}
	if f.err != nil {
		at := f.attempts
		if at == nil {
			at = []upstream.Attempt{{Upstream: "fake", Err: f.err.Error(), Elapsed: time.Millisecond}}
		}
		return nil, "", at, f.err
	}
	at := f.attempts
	if at == nil {
		at = []upstream.Attempt{{Upstream: "fake", Elapsed: time.Millisecond}}
	}
	return f.resp.Copy(), origin, at, nil
}

func (f *fakeExchanger) Close() error { return nil }

// mockWriter — ResponseWriter, сохраняющий записанный ответ.
type mockWriter struct {
	msg    *dns.Msg
	remote net.Addr
}

func (m *mockWriter) LocalAddr() net.Addr         { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53} }
func (m *mockWriter) WriteMsg(msg *dns.Msg) error { m.msg = msg; return nil }
func (m *mockWriter) Write(b []byte) (int, error) { return len(b), nil }
func (m *mockWriter) Close() error                { return nil }
func (m *mockWriter) TsigStatus() error           { return nil }
func (m *mockWriter) TsigTimersOnly(bool)         {}
func (m *mockWriter) Hijack()                     {}

func (m *mockWriter) RemoteAddr() net.Addr {
	if m.remote != nil {
		return m.remote
	}
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
}

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

func aResponse(name string, ttl uint32) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.Response = true
	rr, _ := dns.NewRR(name + ". 5 IN A 1.2.3.4")
	rr.Header().Ttl = ttl
	m.Answer = append(m.Answer, rr)
	return m
}

func testState(fa, fb exchanger, minTTL uint32) *state {
	return &state{
		matcher:      rules.New([]config.Rule{{Zone: "ru", Group: "b"}}),
		groups:       map[string]exchanger{"a": fa, "b": fb},
		def:          fa,
		defName:      "a",
		budget:       time.Second,
		cacheEnabled: true,
		minTTL:       minTTL,
	}
}

func newTestServer(st *state, c *cache.Cache, ss *stats.Stats) *Server {
	s := &Server{cache: c, stats: ss, log: slog.Default()}
	s.st.Store(st)
	return s
}

func TestServeDNSMissThenHit(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	c := cache.New(100, 60, 0, 0)
	ss := stats.New(0)
	s := newTestServer(testState(fa, fa, 60), c, ss)

	// Первый запрос — промах, идёт к апстриму.
	w := &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeA))
	if w.msg == nil || w.msg.Rcode != dns.RcodeSuccess || len(w.msg.Answer) != 1 {
		t.Fatalf("bad reply: %+v", w.msg)
	}
	if fa.calls != 1 {
		t.Fatalf("calls = %d, want 1", fa.calls)
	}
	if !fa.last.RecursionDesired {
		t.Fatalf("upstream query must have RD set")
	}
	if c.Len() != 1 {
		t.Fatalf("cache len = %d, want 1", c.Len())
	}

	// Второй запрос — попадание в кэш, апстрим не вызывается.
	w2 := &mockWriter{}
	s.ServeDNS(w2, query("example.com", dns.TypeA))
	if fa.calls != 1 {
		t.Fatalf("cache miss on second query, calls = %d", fa.calls)
	}
	snap := ss.Snapshot()
	if snap.CacheHits != 1 || snap.CacheMisses != 1 {
		t.Fatalf("hits/misses = %d/%d", snap.CacheHits, snap.CacheMisses)
	}

	// Последний успешный ответ по домену сохранён (второй запрос из кэша).
	info, ok := ss.DomainInfo("example.com")
	if !ok || info.Answer == nil {
		t.Fatalf("last answer not recorded: %+v", info)
	}
	last := info.Answer
	if last.Rcode != "NOERROR" || !last.FromCache || len(last.Records) != 1 {
		t.Fatalf("last = %+v", last)
	}
	// Даже для ответа из кэша сохраняется исходный апстрим.
	if last.Upstream != "fake" {
		t.Fatalf("cached last upstream = %q, want fake", last.Upstream)
	}

	// Попытка к апстриму учтена ровно один раз (второй запрос — из кэша).
	groups := ss.UpstreamsPage()
	if len(groups) != 1 || groups[0].Name != "a" || groups[0].Total != 2 || groups[0].CacheHits != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if len(groups[0].Upstreams) != 1 || groups[0].Upstreams[0].Upstream != "fake" || groups[0].Upstreams[0].Ok != 1 {
		t.Fatalf("upstreams = %+v", groups[0].Upstreams)
	}
	if p := groups[0].Upstreams[0].P90Ms; p != 1 {
		t.Fatalf("upstream p90 = %v ms, want 1", p)
	}
}

// slowExchanger — exchanger, блокирующий ответ до close(release), с
// потокобезопасным счётчиком обращений.
type slowExchanger struct {
	release chan struct{}
	calls   atomic.Int32
	resp    *dns.Msg
}

func (f *slowExchanger) Exchange(_ context.Context, _ *dns.Msg) (*dns.Msg, string, []upstream.Attempt, error) {
	f.calls.Add(1)
	<-f.release
	return f.resp.Copy(), "fake", []upstream.Attempt{{Upstream: "fake", Elapsed: time.Millisecond}}, nil
}

func (f *slowExchanger) Close() error { return nil }

// TestServeDNSCoalescesConcurrentQueries проверяет singleflight: одинаковые
// одновременные промахи обслуживаются одним обращением к апстриму.
func TestServeDNSCoalescesConcurrentQueries(t *testing.T) {
	fe := &slowExchanger{release: make(chan struct{}), resp: aResponse("example.com", 300)}
	s := newTestServer(testState(fe, fe, 60), cache.New(100, 60, 0, 0), stats.New(0))

	const n = 2
	writers := make([]*mockWriter, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		writers[i] = &mockWriter{}
		wg.Add(1)
		go func(w *mockWriter) {
			defer wg.Done()
			<-start
			s.ServeDNS(w, query("example.com", dns.TypeA))
		}(writers[i])
	}
	close(start)

	// Ждём первого обращения к апстриму, затем даём остальным присоединиться
	// к полёту и отпускаем апстрим.
	deadline := time.Now().Add(2 * time.Second)
	for fe.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(fe.release)
	wg.Wait()

	if got := fe.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (singleflight)", got)
	}
	for i, w := range writers {
		if w.msg == nil || len(w.msg.Answer) != 1 {
			t.Fatalf("writer %d reply = %+v", i, w.msg)
		}
	}
}

func TestServeDNSAppliesMinTTL(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 5)}
	s := newTestServer(testState(fa, fa, 60), cache.New(100, 60, 0, 0), stats.New(0))

	w := &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeA))
	if got := w.msg.Answer[0].Header().Ttl; got != 60 {
		t.Fatalf("ttl = %d, want 60", got)
	}
}

func TestServeDNSRouting(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	fb := &fakeExchanger{resp: aResponse("example.ru", 300)}
	s := newTestServer(testState(fa, fb, 60), cache.New(100, 60, 0, 0), stats.New(0))

	// Зона .ru -> группа b.
	s.ServeDNS(&mockWriter{}, query("example.ru", dns.TypeA))
	if fb.calls != 1 || fa.calls != 0 {
		t.Fatalf("routing failed: a=%d b=%d", fa.calls, fb.calls)
	}
	// Всё остальное -> группа по умолчанию a.
	s.ServeDNS(&mockWriter{}, query("example.com", dns.TypeA))
	if fa.calls != 1 {
		t.Fatalf("default routing failed: a=%d", fa.calls)
	}
}

func TestServeDNSServfailOnUpstreamError(t *testing.T) {
	fa := &fakeExchanger{err: errors.New("boom")}
	ss := stats.New(0)
	s := newTestServer(testState(fa, fa, 60), cache.New(100, 60, 0, 0), ss)

	w := &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeA))
	if w.msg.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %d, want SERVFAIL", w.msg.Rcode)
	}
	if ss.Snapshot().Servfail != 1 {
		t.Fatalf("servfail counter not incremented")
	}
	groups := ss.UpstreamsPage()
	if len(groups) != 1 || groups[0].Failed != 1 || len(groups[0].Upstreams) != 1 ||
		groups[0].Upstreams[0].Failed != 1 || groups[0].Upstreams[0].Ok != 0 {
		t.Fatalf("upstream failure not recorded: %+v", groups)
	}
	// Цепочка попыток сохраняется и для SERVFAIL — в последнем ответе домена.
	di, ok := ss.DomainInfo("example.com")
	if !ok || di.Last == nil || len(di.Last.Attempts) != 1 {
		t.Fatalf("servfail attempts not recorded: %+v", di)
	}
	if a := di.Last.Attempts[0]; a.Upstream != "fake" || a.Status != "ERROR" || a.Error != "boom" {
		t.Fatalf("attempt = %+v", a)
	}
}

// TestServeDNSServfailAttemptChain проверяет, что последний ответ домена хранит
// всю цепочку попыток SERVFAIL: DoH-попытку, упавшую по транспорту, и
// UDP-fallback, давший окончательный ответ. Так в деталях домена видно реально
// упавший сервер, а не только адрес последней отвечавшей попытки.
func TestServeDNSServfailAttemptChain(t *testing.T) {
	resp := new(dns.Msg)
	resp.SetQuestion("example.com.", dns.TypeA)
	resp.Response = true
	resp.Rcode = dns.RcodeServerFailure
	fa := &fakeExchanger{
		resp: resp,
		attempts: []upstream.Attempt{
			{Upstream: "https://doh.example/dns-query", Err: "doh https://doh.example: context deadline exceeded", Elapsed: 2 * time.Second},
			{Upstream: "udp://77.88.8.8:53", Rcode: dns.RcodeServerFailure, Elapsed: 10 * time.Millisecond},
		},
	}
	ss := stats.New(0)
	s := newTestServer(testState(fa, fa, 60), nil, ss)

	w := &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeA))
	if w.msg.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %d, want SERVFAIL", w.msg.Rcode)
	}
	di, ok := ss.DomainInfo("example.com")
	if !ok || di.Last == nil {
		t.Fatalf("last answer = %+v", di)
	}
	if got := di.Last.Attempts; len(got) != 2 ||
		got[0].Upstream != "https://doh.example/dns-query" || got[0].Status != "ERROR" ||
		got[1].Upstream != "udp://77.88.8.8:53" || got[1].Status != "SERVFAIL" {
		t.Fatalf("attempts = %+v", got)
	}
}

// TestServeDNSServfailTimeout проверяет, что таймаут апстрима показывается в
// карточке как Reason=Timeout, без длинного текста ошибки в ответе и попытках.
func TestServeDNSServfailTimeout(t *testing.T) {
	timeoutErr := fmt.Errorf("doh https://doh.example/dns-query: %w", context.DeadlineExceeded)
	fa := &fakeExchanger{
		err: timeoutErr,
		attempts: []upstream.Attempt{{
			Upstream: "https://doh.example/dns-query",
			Err:      `Get "https://doh.example/dns-query": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
			Timeout:  true,
			Elapsed:  3 * time.Second,
		}},
	}
	ss := stats.New(0)
	s := newTestServer(testState(fa, fa, 60), nil, ss)

	w := &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeA))
	if w.msg.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %d, want SERVFAIL", w.msg.Rcode)
	}
	di, ok := ss.DomainInfo("example.com")
	if !ok || di.Last == nil {
		t.Fatalf("last answer = %+v", di)
	}
	if !di.Last.Timeout || di.Last.Error != "" {
		t.Fatalf("last = %+v, want Timeout without error text", di.Last)
	}
	if len(di.Last.Attempts) != 1 {
		t.Fatalf("attempts = %+v", di.Last.Attempts)
	}
	if a := di.Last.Attempts[0]; a.Status != "Timeout" || a.Error != "" {
		t.Fatalf("attempt = %+v, want Timeout without error text", a)
	}
	// Для статистики апстримов таймаут по-прежнему считается неудачей.
	groups := ss.UpstreamsPage()
	if len(groups) != 1 || len(groups[0].Upstreams) != 1 || groups[0].Upstreams[0].Failed != 1 {
		t.Fatalf("upstream failure not recorded: %+v", groups)
	}
}

// TestServeDNSRecordsNoDataAttempts проверяет, что NODATA-ответ попадает в
// сводку как негативный (Status NODATA) вместе с цепочкой пройденных апстримов.
func TestServeDNSRecordsNoDataAttempts(t *testing.T) {
	resp := new(dns.Msg)
	resp.SetQuestion("example.com.", dns.TypeA)
	resp.Response = true // NOERROR без записей — NODATA
	fa := &fakeExchanger{resp: resp, attempts: []upstream.Attempt{
		{Upstream: "udp://192.168.1.1:53", Rcode: dns.RcodeSuccess, Nodata: true, Elapsed: 4 * time.Millisecond},
		{Upstream: "doh+https://9.9.9.9/dns-query", Rcode: dns.RcodeSuccess, Nodata: true, Elapsed: 40 * time.Millisecond},
	}}
	ss := stats.New(0)
	s := newTestServer(testState(fa, fa, 60), cache.New(100, 60, 0, 0), ss)

	s.ServeDNS(&mockWriter{}, query("example.com", dns.TypeA))

	di, ok := ss.DomainInfo("example.com")
	if !ok {
		t.Fatal("domain not found")
	}
	if di.Answer != nil {
		t.Fatalf("NODATA must not produce successful answer: %+v", di.Answer)
	}
	if len(di.Negative) != 1 {
		t.Fatalf("negative = %+v", di.Negative)
	}
	neg := di.Negative[0]
	if neg.Rcode != "NOERROR" || neg.Qtype != "A" {
		t.Fatalf("negative = %+v", neg)
	}
	if neg.Upstream != "doh+https://9.9.9.9/dns-query" {
		t.Fatalf("negative upstream = %q", neg.Upstream)
	}
	if len(neg.Attempts) != 2 {
		t.Fatalf("attempts = %+v", neg.Attempts)
	}
	if neg.Attempts[0].Status != "NODATA" || neg.Attempts[0].Upstream != "udp://192.168.1.1:53" {
		t.Fatalf("attempt[0] = %+v", neg.Attempts[0])
	}
	if neg.Attempts[1].Status != "NODATA" || neg.Attempts[1].DurationMs != 40 {
		t.Fatalf("attempt[1] = %+v", neg.Attempts[1])
	}
}

// TestServeDNSPositiveAfterNoDataKeepsChain проверяет, что у успешного ответа,
// полученного после NODATA основного сервера, сохраняется вся цепочка попыток.
func TestServeDNSPositiveAfterNoDataKeepsChain(t *testing.T) {
	fa := &fakeExchanger{
		resp: aResponse("example.com", 300),
		attempts: []upstream.Attempt{
			{Upstream: "udp://192.168.1.1:53", Rcode: dns.RcodeSuccess, Nodata: true, Elapsed: 4 * time.Millisecond},
			{Upstream: "doh+https://9.9.9.9/dns-query", Rcode: dns.RcodeSuccess, Elapsed: 40 * time.Millisecond},
		},
	}
	ss := stats.New(0)
	s := newTestServer(testState(fa, fa, 60), cache.New(100, 60, 0, 0), ss)

	s.ServeDNS(&mockWriter{}, query("example.com", dns.TypeA))

	di, ok := ss.DomainInfo("example.com")
	if !ok || di.Answer == nil {
		t.Fatalf("answer not recorded: %+v", di)
	}
	if len(di.Negative) != 0 {
		t.Fatalf("negative must be empty: %+v", di.Negative)
	}
	if len(di.Answer.Attempts) != 2 || di.Answer.Attempts[0].Status != "NODATA" || di.Answer.Attempts[1].Status != "OK" {
		t.Fatalf("attempts = %+v", di.Answer.Attempts)
	}
}

func TestServeDNSRestoresIDAndQuestion(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	fa.resp.Question = nil // апстрим не вернул вопрос
	fa.resp.Id = 999
	s := newTestServer(testState(fa, fa, 60), nil, stats.New(0))

	q := query("example.com", dns.TypeA)
	q.Id = 4242
	w := &mockWriter{}
	s.ServeDNS(w, q)
	if w.msg.Id != 4242 {
		t.Fatalf("id = %d, want 4242", w.msg.Id)
	}
	if len(w.msg.Question) != 1 || w.msg.Question[0].Name != "example.com." {
		t.Fatalf("question not restored: %+v", w.msg.Question)
	}
}

func TestServeDNSRefusedOnBadQuery(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	s := newTestServer(testState(fa, fa, 60), nil, stats.New(0))

	m := new(dns.Msg)
	m.Id = 1
	m.Response = false
	w := &mockWriter{}
	s.ServeDNS(w, m) // нет вопроса
	if w.msg.Rcode != dns.RcodeRefused {
		t.Fatalf("rcode = %d, want REFUSED", w.msg.Rcode)
	}
	if fa.calls != 0 {
		t.Fatalf("upstream called for bad query")
	}
}

func TestTruncationUDPvsTCP(t *testing.T) {
	resp := new(dns.Msg)
	resp.SetQuestion("example.com.", dns.TypeA)
	resp.Response = true
	for i := 0; i < 100; i++ {
		rr, _ := dns.NewRR("example.com. 300 IN A 10.0.0.1")
		resp.Answer = append(resp.Answer, rr)
	}
	fa := &fakeExchanger{resp: resp}
	s := newTestServer(testState(fa, fa, 0), cache.New(100, 0, 0, 0), stats.New(0))

	// UDP без EDNS: обрезаем до 512 с TC=1.
	wu := &mockWriter{}
	s.ServeDNS(wu, query("example.com", dns.TypeA))
	if !wu.msg.Truncated {
		t.Fatalf("UDP reply must be truncated")
	}
	packed, _ := wu.msg.Pack()
	if len(packed) > dns.MinMsgSize {
		t.Fatalf("UDP reply size = %d, want <= %d", len(packed), dns.MinMsgSize)
	}

	// TCP: без обрезки.
	wt := &mockWriter{remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}}
	s.ServeDNS(wt, query("example.com", dns.TypeA))
	if wt.msg.Truncated || len(wt.msg.Answer) != 100 {
		t.Fatalf("TCP reply truncated: tc=%v answers=%d", wt.msg.Truncated, len(wt.msg.Answer))
	}
}

func TestReloadSwapsState(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	s := newTestServer(testState(fa, fa, 60), nil, stats.New(0))

	old := s.st.Load()
	cfg := config.Default()
	if err := s.Reload(cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s.st.Load() == old {
		t.Fatalf("state not swapped")
	}
}

// TestBuildStateRequestBudget проверяет, что общий бюджет запроса вмещает все
// попытки группы (primary + fallback), иначе fallback не успевает сработать.
func TestBuildStateRequestBudget(t *testing.T) {
	cfg, err := config.Parse([]byte(`
upstream: {timeout: 2s}
groups:
  a:
    protocol: udp
    primary: ["192.168.1.1:53"]
    fallback: ["doh+https://9.9.9.9/dns-query", "doh+https://94.140.14.14/dns-query"]
default_group: a
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st, err := buildState(cfg, cfg.Timeout(), false)
	if err != nil {
		t.Fatalf("buildState: %v", err)
	}
	defer closeState(st)
	// 1 primary + 2 fallback = 3 попытки → бюджет = 3 × 2s.
	if st.budget != 6*time.Second {
		t.Fatalf("budget = %s, want 6s", st.budget)
	}
}
