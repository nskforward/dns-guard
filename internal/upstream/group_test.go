package upstream

import (
	"context"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/nskforward/dns-guard/internal/config"
)

// TestNewProtocolPrefix проверяет, что префикс в адресе перекрывает протокол группы.
func TestNewProtocolPrefix(t *testing.T) {
	r, err := New("udp", "doh+https://9.9.9.9/dns-query", nil, nil, time.Second)
	if err != nil {
		t.Fatalf("new doh via prefix: %v", err)
	}
	if _, ok := r.(*dohResolver); !ok {
		t.Fatalf("want *dohResolver, got %T", r)
	}

	r2, err := New("doh", "udp+192.168.1.1:53", nil, nil, time.Second)
	if err != nil {
		t.Fatalf("new udp via prefix: %v", err)
	}
	if _, ok := r2.(*udpResolver); !ok {
		t.Fatalf("want *udpResolver, got %T", r2)
	}

	if _, err := New("udp", "tcp+1.1.1.1", nil, nil, time.Second); err == nil {
		t.Fatal("unknown prefix: expected error")
	}
}

// TestGroupMixedProtocolFallback проверяет группу, где fallback задан другим
// протоколом через префикс: UDP-primary недоступен, ответ приходит от DoH-fallback.
func TestGroupMixedProtocolFallback(t *testing.T) {
	srv := newDoHTestServer(t)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	_, port, _ := net.SplitHostPort(u.Host)

	g := config.Group{
		Protocol: "udp",
		Primary:  []string{"127.0.0.1:1"}, // заведомо недоступный UDP
		Fallback: []string{"doh+http://127.0.0.1:" + port + "/dns-query"},
	}
	grp, err := NewGroup("mixed", g, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("new group: %v", err)
	}
	defer grp.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, addr, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answer = %v, want 1 record", resp.Answer)
	}
	if !strings.HasPrefix(addr, "http") {
		t.Fatalf("ответил %q, ожидался DoH-fallback", addr)
	}
}
