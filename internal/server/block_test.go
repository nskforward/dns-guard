package server

import (
	"net"
	"testing"

	"github.com/miekg/dns"
	"github.com/nskforward/dns-guard/internal/cache"
	"github.com/nskforward/dns-guard/internal/config"
	"github.com/nskforward/dns-guard/internal/rules"
	"github.com/nskforward/dns-guard/internal/stats"
)

// testBlockedState — состояние сервера с заданным блоклистом.
func testBlockedState(fa exchanger, mode string, domains ...string) *state {
	st := testState(fa, fa, 0)
	st.block = rules.NewBlocklist(domains)
	st.blockMode = mode
	return st
}

func TestServeDNSBlockedNoData(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	ss := stats.New(0)
	c := cache.New(100, 60, 0, 0)
	s := newTestServer(testBlockedState(fa, config.BlockModeNoData, "example.com"), c, ss)

	w := &mockWriter{}
	s.ServeDNS(w, query("a.b.example.com", dns.TypeA))

	if w.msg == nil || w.msg.Rcode != dns.RcodeSuccess || len(w.msg.Answer) != 0 {
		t.Fatalf("bad blocked reply: %+v", w.msg)
	}
	if fa.calls != 0 {
		t.Fatalf("upstream called for blocked domain: %d", fa.calls)
	}
	if c.Len() != 0 {
		t.Fatalf("blocked answer must not be cached")
	}
	snap := ss.Snapshot()
	if snap.Blocked != 1 || snap.CacheHits != 0 || snap.CacheMisses != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
	info, ok := ss.DomainInfo("a.b.example.com")
	if !ok {
		t.Fatalf("blocked domain not recorded: %+v", info)
	}
	if len(info.Answers) != 0 {
		t.Fatalf("blocked answers must not be listed per type: %+v", info.Answers)
	}
	if info.Last == nil || !info.Last.Blocked || info.Last.BlockedBy != "example.com" {
		t.Fatalf("last blocked answer = %+v", info.Last)
	}
}

func TestServeDNSBlockedZero(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	s := newTestServer(testBlockedState(fa, config.BlockModeZero, "example.com"), nil, stats.New(0))

	w := &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeA))
	if len(w.msg.Answer) != 1 {
		t.Fatalf("A answer = %+v", w.msg.Answer)
	}
	a, ok := w.msg.Answer[0].(*dns.A)
	if !ok || !a.A.Equal(net.IPv4zero) {
		t.Fatalf("A record = %+v, want 0.0.0.0", w.msg.Answer[0])
	}

	w = &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeAAAA))
	if len(w.msg.Answer) != 1 {
		t.Fatalf("AAAA answer = %+v", w.msg.Answer)
	}
	aaaa, ok := w.msg.Answer[0].(*dns.AAAA)
	if !ok || !aaaa.AAAA.Equal(net.IPv6zero) {
		t.Fatalf("AAAA record = %+v, want ::", w.msg.Answer[0])
	}

	// Остальные типы в режиме zero получают пустой NOERROR.
	w = &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeMX))
	if w.msg.Rcode != dns.RcodeSuccess || len(w.msg.Answer) != 0 {
		t.Fatalf("MX answer = %+v", w.msg)
	}
	if fa.calls != 0 {
		t.Fatalf("upstream called for blocked domain")
	}
}

// TestBlockedOverridesCachedAnswer проверяет, что блоклист проверяется до кэша:
// ответ, закэшированный до блокировки, не обходит её.
func TestBlockedOverridesCachedAnswer(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	c := cache.New(100, 60, 0, 0)
	s := newTestServer(testState(fa, fa, 60), c, stats.New(0))

	s.ServeDNS(&mockWriter{}, query("example.com", dns.TypeA))
	if c.Len() != 1 {
		t.Fatalf("cache len = %d, want 1", c.Len())
	}

	s.st.Store(testBlockedState(fa, config.BlockModeNoData, "example.com"))
	w := &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeA))
	if len(w.msg.Answer) != 0 {
		t.Fatalf("cached answer leaked through block: %+v", w.msg.Answer)
	}
	if fa.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", fa.calls)
	}
}

// TestReloadFlushesCacheOnBlockChange проверяет, что смена блоклиста сбрасывает
// кэш, чтобы старые положительные ответы не пережили блокировку.
func TestReloadFlushesCacheOnBlockChange(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	c := cache.New(100, 60, 0, 0)
	s := newTestServer(testState(fa, fa, 60), c, stats.New(0))

	q := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	c.Put(q, false, aResponse("example.com", 300), "fake")
	if c.Len() != 1 {
		t.Fatalf("cache len = %d, want 1", c.Len())
	}

	cfg := config.Default()
	cfg.Block.Domains = []string{"example.com"}
	if err := s.Reload(cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c.Len() != 0 {
		t.Fatalf("cache not flushed after blocklist change: len = %d", c.Len())
	}
}
