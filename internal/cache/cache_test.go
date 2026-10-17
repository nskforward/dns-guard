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
	c := newWithClock(100, 60, 0, func() time.Time { return now })

	m, q := msgA("example.com", 5)
	c.Put(q, m, "8.8.8.8:53")

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
	c := newWithClock(10, 0, 30, func() time.Time { return now })
	m, q := msgA("example.com", 3600)
	c.Put(q, m, "")
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
	c := newWithClock(10, 0, 0, func() time.Time { return now })

	q := dns.Question{Name: "nx.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	m := new(dns.Msg)
	m.Question = []dns.Question{q}
	m.Response = true
	m.Rcode = dns.RcodeNameError
	soa, _ := dns.NewRR("example.com. 100 IN SOA ns.example.com. host.example.com. 1 2 3 4 30")
	m.Ns = []dns.RR{soa}
	c.Put(q, m, "")

	if _, _, ok := c.Get(q, false); !ok {
		t.Fatal("negative response not cached")
	}
	now = time.Unix(31, 0) // SOA minttl = 30
	if _, _, ok := c.Get(q, false); ok {
		t.Fatal("negative entry must expire after SOA minttl")
	}
}

func TestSkipTruncatedAndError(t *testing.T) {
	c := newWithClock(10, 0, 0, time.Now)
	m, q := msgA("example.com", 300)
	m.Truncated = true
	c.Put(q, m, "")
	if c.Len() != 0 {
		t.Fatal("truncated response must not be cached")
	}

	m2, q2 := msgA("other.com", 300)
	m2.Rcode = dns.RcodeServerFailure
	c.Put(q2, m2, "")
	if c.Len() != 0 {
		t.Fatal("SERVFAIL must not be cached")
	}
}

func TestLRUEvictionAndFlush(t *testing.T) {
	c := newWithClock(2, 0, 0, time.Now)
	for _, n := range []string{"a.com", "b.com", "c.com"} {
		m, q := msgA(n, 300)
		c.Put(q, m, "")
	}
	if c.Len() != 2 {
		t.Fatalf("len = %d, want 2", c.Len())
	}
	c.Flush()
	if c.Len() != 0 {
		t.Fatalf("len after flush = %d, want 0", c.Len())
	}
}
