package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/miekg/dns"
	"github.com/nskforward/dns-guard/internal/cache"
	"github.com/nskforward/dns-guard/internal/config"
	"github.com/nskforward/dns-guard/internal/stats"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse CIDR %q: %v", s, err)
	}
	return n
}

// ecsOptions возвращает ECS-опции сообщения.
func ecsOptions(m *dns.Msg) []*dns.EDNS0_SUBNET {
	var out []*dns.EDNS0_SUBNET
	if opt := m.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if s, ok := o.(*dns.EDNS0_SUBNET); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func TestSetECSIPv4(t *testing.T) {
	nets := ecsNets{v4: mustCIDR(t, "203.0.113.0/24")}
	m := query("example.com", dns.TypeA)
	setECS(m, nets, dns.TypeA)

	opts := ecsOptions(m)
	if len(opts) != 1 {
		t.Fatalf("ecs options = %d, want 1", len(opts))
	}
	e := opts[0]
	if e.Family != 1 || e.SourceNetmask != 24 || e.SourceScope != 0 {
		t.Fatalf("bad ecs option: %+v", e)
	}
	if !e.Address.Equal(net.ParseIP("203.0.113.0")) {
		t.Fatalf("address = %s, want 203.0.113.0", e.Address)
	}
	// OPT добавлен с разумным размером UDP.
	if got := m.IsEdns0().UDPSize(); got != 1232 {
		t.Fatalf("udp size = %d, want 1232", got)
	}
}

func TestSetECSReplacesClientOption(t *testing.T) {
	nets := ecsNets{v4: mustCIDR(t, "203.0.113.0/24")}
	m := query("example.com", dns.TypeA)
	m.SetEdns0(4096, true)
	m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        1,
		SourceNetmask: 8,
		Address:       net.ParseIP("10.0.0.0").To4(),
	})

	setECS(m, nets, dns.TypeA)

	opts := ecsOptions(m)
	if len(opts) != 1 {
		t.Fatalf("ecs options = %d, want 1 (клиентская должна быть заменена)", len(opts))
	}
	if !opts[0].Address.Equal(net.ParseIP("203.0.113.0")) {
		t.Fatalf("address = %s, want ours", opts[0].Address)
	}
	if opt := m.IsEdns0(); opt.UDPSize() != 4096 || !opt.Do() {
		t.Fatalf("client EDNS settings lost: size=%d do=%v", opt.UDPSize(), opt.Do())
	}
}

func TestSetECSFamilies(t *testing.T) {
	both := ecsNets{
		v4: mustCIDR(t, "203.0.113.0/24"),
		v6: mustCIDR(t, "2001:db8::/56"),
	}
	// AAAA — IPv6-подсеть.
	m := query("example.com", dns.TypeAAAA)
	setECS(m, both, dns.TypeAAAA)
	if got := ecsOptions(m); len(got) != 1 || got[0].Family != 2 || got[0].SourceNetmask != 56 {
		t.Fatalf("AAAA ecs = %+v, want family 2 /56", got)
	}
	// AAAA без IPv6-подсети — откат на IPv4.
	m = query("example.com", dns.TypeAAAA)
	setECS(m, ecsNets{v4: both.v4}, dns.TypeAAAA)
	if got := ecsOptions(m); len(got) != 1 || got[0].Family != 1 {
		t.Fatalf("AAAA fallback ecs = %+v, want family 1", got)
	}
	// Прочие типы — IPv4, а без неё — IPv6.
	m = query("example.com", dns.TypeHTTPS)
	setECS(m, both, dns.TypeHTTPS)
	if got := ecsOptions(m); len(got) != 1 || got[0].Family != 1 {
		t.Fatalf("HTTPS ecs = %+v, want family 1", got)
	}
	m = query("example.com", dns.TypeHTTPS)
	setECS(m, ecsNets{v6: both.v6}, dns.TypeHTTPS)
	if got := ecsOptions(m); len(got) != 1 || got[0].Family != 2 {
		t.Fatalf("HTTPS v6-only ecs = %+v, want family 2", got)
	}
}

func TestSetECSNoSubnet(t *testing.T) {
	m := query("example.com", dns.TypeA)
	setECS(m, ecsNets{}, dns.TypeA)
	if m.IsEdns0() != nil {
		t.Fatal("OPT must not be added when no ECS subnet configured")
	}
}

func TestFitEDNS(t *testing.T) {
	resp := aResponse("example.com", 300)
	resp.SetEdns0(1232, false)
	rr, _ := dns.NewRR("example.com. 5 IN A 5.6.7.8")
	resp.Extra = append(resp.Extra, rr)

	// Клиент без EDNS — OPT удаляется, прочие Extra сохраняются.
	out := resp.Copy()
	fitEDNS(out, query("example.com", dns.TypeA))
	if out.IsEdns0() != nil {
		t.Fatal("OPT must be stripped for non-EDNS client")
	}
	if len(out.Extra) != 1 {
		t.Fatalf("extra = %d, want 1", len(out.Extra))
	}

	// Клиент с EDNS — OPT сохраняется.
	out = resp.Copy()
	req := query("example.com", dns.TypeA)
	req.SetEdns0(4096, false)
	fitEDNS(out, req)
	if out.IsEdns0() == nil {
		t.Fatal("OPT must be kept for EDNS client")
	}
}

func TestServeDNSAppliesECSAndStripsOPT(t *testing.T) {
	resp := aResponse("example.com", 300)
	resp.SetEdns0(1232, false)
	fa := &fakeExchanger{resp: resp}
	c := cache.New(100, 60, 0, 0)
	st := testState(fa, fa, 60)
	st.ecs = config.ECS{Enabled: true}
	s := newTestServer(st, c, stats.New(0))
	s.ecsNets = ecsNets{v4: mustCIDR(t, "203.0.113.0/24")}

	// Промах: к апстриму уходит ECS, клиенту OPT не возвращается.
	w := &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeA))
	if fa.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", fa.calls)
	}
	opts := ecsOptions(fa.last)
	if len(opts) != 1 || opts[0].SourceNetmask != 24 {
		t.Fatalf("outgoing ecs = %+v, want /24", opts)
	}
	if w.msg == nil || w.msg.IsEdns0() != nil {
		t.Fatal("client without EDNS must not receive OPT")
	}

	// Хит кэша: OPT также вырезан, апстрим не опрашивается.
	w = &mockWriter{}
	s.ServeDNS(w, query("example.com", dns.TypeA))
	if fa.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (cache hit)", fa.calls)
	}
	if w.msg == nil || w.msg.IsEdns0() != nil {
		t.Fatal("cache hit with non-EDNS client must not contain OPT")
	}
}

func TestServeDNSECSGroupOverride(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	st := testState(fa, fa, 60)
	st.ecs = config.ECS{Enabled: true}
	st.ecsOff = map[string]bool{"a": true}
	s := newTestServer(st, cache.New(100, 60, 0, 0), stats.New(0))
	s.ecsNets = ecsNets{v4: mustCIDR(t, "203.0.113.0/24")}

	s.ServeDNS(&mockWriter{}, query("example.com", dns.TypeA))
	if opts := ecsOptions(fa.last); len(opts) != 0 {
		t.Fatalf("group override off: unexpected ecs %+v", opts)
	}
}

func TestServeDNSECSDisabled(t *testing.T) {
	fa := &fakeExchanger{resp: aResponse("example.com", 300)}
	s := newTestServer(testState(fa, fa, 60), cache.New(100, 60, 0, 0), stats.New(0))
	s.ecsNets = ecsNets{v4: mustCIDR(t, "203.0.113.0/24")}

	s.ServeDNS(&mockWriter{}, query("example.com", dns.TypeA))
	if opts := ecsOptions(fa.last); len(opts) != 0 {
		t.Fatalf("ecs disabled: unexpected ecs %+v", opts)
	}
}

func TestDetectOnceUpdatesSubnetAndFlushesCache(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "198.51.100.7\n")
	}))
	defer ts.Close()

	c := cache.New(100, 60, 0, 0)
	q := query("example.com", dns.TypeA)
	c.Put(q.Question[0], false, aResponse("example.com", 300), "upstream")

	s := &Server{log: slog.Default(), cache: c}
	s.ecsGen.Store(1)
	s.detectOnce(context.Background(), config.ECS{
		Enabled:    true,
		Autodetect: config.ECSAutodetect{URL: ts.URL},
	}, 1)

	s.ecsMu.RLock()
	got := netString(s.ecsNets.v4)
	s.ecsMu.RUnlock()
	if got != "198.51.100.0/24" {
		t.Fatalf("detected subnet = %q, want 198.51.100.0/24", got)
	}
	if c.Len() != 0 {
		t.Fatalf("cache len = %d, want 0 (flush on subnet change)", c.Len())
	}
}

func TestDetectOnceStaleGeneration(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "198.51.100.7")
	}))
	defer ts.Close()

	s := &Server{log: slog.Default()}
	s.ecsGen.Store(2)
	s.detectOnce(context.Background(), config.ECS{Autodetect: config.ECSAutodetect{URL: ts.URL}}, 1)

	s.ecsMu.RLock()
	defer s.ecsMu.RUnlock()
	if s.ecsNets.v4 != nil {
		t.Fatalf("stale detector applied subnet %s", s.ecsNets.v4)
	}
}

func TestDetectOnceKeepsStaticOnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer ts.Close()

	s := &Server{log: slog.Default()}
	s.ecsGen.Store(1)
	s.ecsStatic = ecsNets{v4: mustCIDR(t, "203.0.113.0/24")}
	s.ecsNets = s.ecsStatic

	s.detectOnce(context.Background(), config.ECS{Autodetect: config.ECSAutodetect{URL: ts.URL}}, 1)

	s.ecsMu.RLock()
	defer s.ecsMu.RUnlock()
	if netString(s.ecsNets.v4) != "203.0.113.0/24" {
		t.Fatalf("static fallback lost: %v", s.ecsNets.v4)
	}
}

func TestReloadFlushesCacheOnECSChange(t *testing.T) {
	mk := func(subnet string) *config.Config {
		cfg, err := config.Parse([]byte(`
cache: {enabled: true, min_ttl: 60}
ecs: {enabled: true, subnet: "` + subnet + `"}
groups:
  g1: {protocol: udp, primary: ["1.1.1.1"]}
default_group: g1
`))
		if err != nil {
			t.Fatalf("parse config: %v", err)
		}
		return cfg
	}

	c := cache.New(100, 60, 0, 0)
	put := func() {
		c.Put(query("example.com", dns.TypeA).Question[0], false, aResponse("example.com", 300), "upstream")
	}

	s, err := New(mk("203.0.113.0/24"), c, stats.New(0), slog.Default())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	put()
	if err := s.Reload(mk("198.51.100.0/24")); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c.Len() != 0 {
		t.Fatalf("cache len = %d, want 0 (flush on ECS subnet change)", c.Len())
	}

	// Неизменные ECS-настройки кэш не трогают.
	put()
	if err := s.Reload(mk("198.51.100.0/24")); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c.Len() != 1 {
		t.Fatalf("cache len = %d, want 1 (no flush without changes)", c.Len())
	}
}
