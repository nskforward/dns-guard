package cache

import (
	"fmt"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func msgA(name string, ttl uint32) (*dns.Msg, dns.Question) {
	q := dns.Question{Name: dns.Fqdn(name), Qtype: dns.TypeA, Qclass: dns.ClassINET}
	m := new(dns.Msg)
	m.Question = []dns.Question{q}
	m.Response = true
	rr, _ := dns.NewRR(fmt.Sprintf("%s %d IN A 1.2.3.4", dns.Fqdn(name), ttl))
	m.Answer = []dns.RR{rr}
	return m, q
}

func TestMinTTLAndDecay(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newWithClock(100, 60, 0, 0, func() time.Time { return now })

	m, q := msgA("example.com", 5)
	c.Put(q, false, m, "8.8.8.8:53")

	got, origin, ok := c.Get(q, false)
	if !ok {
		t.Fatal("cache miss, want hit")
	}
	if origin != "8.8.8.8:53" {
		t.Fatalf("origin = %q, want 8.8.8.8:53", origin)
	}
	if ttl := got.Answer[0].Header().Ttl; ttl != 60 {
		t.Fatalf("ttl = %d, want 60 (MIN-TTL)", ttl)
	}

	now = time.Unix(1015, 0) // +15s
	got, _, ok = c.Get(q, false)
	if !ok {
		t.Fatal("cache miss after 15s")
	}
	if ttl := got.Answer[0].Header().Ttl; ttl != 45 {
		t.Fatalf("ttl = %d, want 45", ttl)
	}

	now = time.Unix(1061, 0) // +61s -> истёк
	if _, _, ok := c.Get(q, false); ok {
		t.Fatal("entry must be expired")
	}
}

func TestMaxTTL(t *testing.T) {
	now := time.Unix(0, 0)
	c := newWithClock(10, 0, 30, 0, func() time.Time { return now })
	m, q := msgA("example.com", 3600)
	c.Put(q, false, m, "")
	got, _, ok := c.Get(q, false)
	if !ok {
		t.Fatal("miss")
	}
	if ttl := got.Answer[0].Header().Ttl; ttl != 30 {
		t.Fatalf("ttl = %d, want 30 (MAX-TTL)", ttl)
	}
}

func TestNegativeCaching(t *testing.T) {
	now := time.Unix(0, 0)
	c := newWithClock(10, 0, 0, 0, func() time.Time { return now })

	q := dns.Question{Name: "nx.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	m := new(dns.Msg)
	m.Question = []dns.Question{q}
	m.Response = true
	m.Rcode = dns.RcodeNameError
	soa, _ := dns.NewRR("example.com. 100 IN SOA ns.example.com. host.example.com. 1 2 3 4 30")
	m.Ns = []dns.RR{soa}
	c.Put(q, false, m, "")

	if _, _, ok := c.Get(q, false); !ok {
		t.Fatal("negative response not cached")
	}
	now = time.Unix(31, 0) // SOA minttl = 30
	if _, _, ok := c.Get(q, false); ok {
		t.Fatal("negative entry must expire after SOA minttl")
	}
}

func TestSkipTruncatedAndError(t *testing.T) {
	c := newWithClock(10, 0, 0, 0, time.Now)
	m, q := msgA("example.com", 300)
	m.Truncated = true
	c.Put(q, false, m, "")
	if c.Len() != 0 {
		t.Fatal("truncated response must not be cached")
	}

	m2, q2 := msgA("other.com", 300)
	m2.Rcode = dns.RcodeServerFailure
	c.Put(q2, false, m2, "")
	if c.Len() != 0 {
		t.Fatal("SERVFAIL must not be cached")
	}
}

func TestNegativeTTLCap(t *testing.T) {
	now := time.Unix(0, 0)
	c := newWithClock(10, 0, 0, 10, func() time.Time { return now })

	q := dns.Question{Name: "nx.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	m := new(dns.Msg)
	m.Question = []dns.Question{q}
	m.Response = true
	m.Rcode = dns.RcodeNameError
	soa, _ := dns.NewRR("example.com. 3600 IN SOA ns.example.com. host.example.com. 1 2 3 4 3600")
	m.Ns = []dns.RR{soa}
	c.Put(q, false, m, "")

	now = time.Unix(9, 0)
	if _, _, ok := c.Get(q, false); !ok {
		t.Fatal("negative entry must be cached within neg_ttl cap")
	}
	now = time.Unix(11, 0)
	if _, _, ok := c.Get(q, false); ok {
		t.Fatal("negative entry must expire after neg_ttl cap, not after SOA minttl")
	}
}

// TestNoDataNormalTypesMinTTL проверяет, что NODATA для AAAA/HTTPS/SVCB
// кэшируется не меньше MIN-TTL, даже если SOA minttl меньше, а при отсутствии
// SOA — ровно MIN-TTL.
func TestNoDataNormalTypesMinTTL(t *testing.T) {
	for _, qtype := range []uint16{dns.TypeAAAA, dns.TypeHTTPS, dns.TypeSVCB} {
		now := time.Unix(0, 0)
		c := newWithClock(10, 60, 0, 0, func() time.Time { return now })
		m, q := noDataMsg("example.com", qtype, 30, 30)
		c.Put(q, false, m, "")

		now = time.Unix(59, 0)
		if _, _, ok := c.Get(q, false); !ok {
			t.Fatalf("type %d: entry must be cached for MIN-TTL", qtype)
		}
		now = time.Unix(61, 0)
		if _, _, ok := c.Get(q, false); ok {
			t.Fatalf("type %d: entry must expire after MIN-TTL", qtype)
		}
	}

	// NODATA без SOA тоже кэшируется на MIN-TTL.
	now := time.Unix(0, 0)
	c := newWithClock(10, 60, 0, 0, func() time.Time { return now })
	q := dns.Question{Name: "example.com.", Qtype: dns.TypeHTTPS, Qclass: dns.ClassINET}
	m := new(dns.Msg)
	m.Question = []dns.Question{q}
	m.Response = true
	c.Put(q, false, m, "")
	now = time.Unix(59, 0)
	if _, _, ok := c.Get(q, false); !ok {
		t.Fatal("NODATA without SOA must be cached for MIN-TTL")
	}
}

// TestNoDataNormalTypesNegTTLCap проверяет, что потолок neg_ttl применяется к
// NODATA для AAAA/HTTPS/SVCB и имеет приоритет над MIN-TTL.
func TestNoDataNormalTypesNegTTLCap(t *testing.T) {
	now := time.Unix(0, 0)
	c := newWithClock(10, 60, 0, 10, func() time.Time { return now })
	m, q := noDataMsg("example.com", dns.TypeHTTPS, 3600, 3600)
	c.Put(q, false, m, "")

	now = time.Unix(9, 0)
	if _, _, ok := c.Get(q, false); !ok {
		t.Fatal("entry must be cached within neg_ttl cap")
	}
	now = time.Unix(11, 0)
	if _, _, ok := c.Get(q, false); ok {
		t.Fatal("entry must expire after neg_ttl cap, not after MIN-TTL")
	}
}

// noDataMsg формирует NODATA-ответ (NOERROR без записей) с SOA в authority.
func noDataMsg(name string, qtype uint16, soaTTL, soaMinTTL uint32) (*dns.Msg, dns.Question) {
	fqdn := dns.Fqdn(name)
	q := dns.Question{Name: fqdn, Qtype: qtype, Qclass: dns.ClassINET}
	m := new(dns.Msg)
	m.Question = []dns.Question{q}
	m.Response = true
	soa, _ := dns.NewRR(fmt.Sprintf("%s %d IN SOA ns.%s hostmaster.%s 1 2 3 4 %d",
		fqdn, soaTTL, fqdn, fqdn, soaMinTTL))
	m.Ns = []dns.RR{soa}
	return m, q
}

// TestPutUsesClientQuestionAndDO проверяет, что ключ кэша строится по вопросу
// клиента, а не по вопросу, который эхоил апстрим.
func TestPutUsesClientQuestionAndDO(t *testing.T) {
	c := newWithClock(10, 0, 0, 0, time.Now)
	q := dns.Question{Name: "Example.COM.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	m, _ := msgA("example.com", 300)
	// Апстрим вернул чужой вопрос — на ключ это влиять не должно.
	m.Question = []dns.Question{{Name: "other.example.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}}
	c.Put(q, false, m, "")

	if _, _, ok := c.Get(q, false); !ok {
		t.Fatal("entry must be keyed by the client's question")
	}
	if _, _, ok := c.Get(m.Question[0], false); ok {
		t.Fatal("upstream echo must not create a separate entry")
	}
	if _, _, ok := c.Get(q, true); ok {
		t.Fatal("different DO bit must use a separate key")
	}
}

func TestLRUEvictionAndFlush(t *testing.T) {
	c := newWithClock(2, 0, 0, 0, time.Now)
	for _, n := range []string{"a.com", "b.com", "c.com"} {
		m, q := msgA(n, 300)
		c.Put(q, false, m, "")
	}
	if c.Len() != 2 {
		t.Fatalf("len = %d, want 2", c.Len())
	}
	c.Flush()
	if c.Len() != 0 {
		t.Fatalf("len after flush = %d, want 0", c.Len())
	}
}
