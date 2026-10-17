package upstream

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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

// newDoHReplyServer поднимает DoH-сервер, отвечающий положительным A-ответом
// (positive=true) либо NODATA с SOA в authority. Возвращает сервер и счётчик
// полученных запросов.
func newDoHReplyServer(t *testing.T, positive bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if err := q.Unpack(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := new(dns.Msg)
		resp.SetReply(q)
		if positive {
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.IPv4(192, 0, 2, 1),
			})
		} else {
			soa, _ := dns.NewRR(q.Question[0].Name + " 60 IN SOA ns. hostmaster. 1 2 3 4 60")
			resp.Ns = append(resp.Ns, soa)
		}
		out, _ := resp.Pack()
		w.Header().Set("Content-Type", dohContentType)
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func dohGroup(primary, fallback *httptest.Server) config.Group {
	g := config.Group{Protocol: "doh", Primary: []string{primary.URL + "/dns-query"}}
	if fallback != nil {
		g.Fallback = []string{fallback.URL + "/dns-query"}
	}
	return g
}

// TestGroupNodataFromPrimaryFallsBack проверяет, что NODATA основного сервера
// проверяется резервным: положительный ответ резерва выигрывает.
func TestGroupNodataFromPrimaryFallsBack(t *testing.T) {
	primary, primaryHits := newDoHReplyServer(t, false)
	fallback, fallbackHits := newDoHReplyServer(t, true)

	grp, err := NewGroup("g", dohGroup(primary, fallback), time.Second)
	if err != nil {
		t.Fatalf("new group: %v", err)
	}
	defer grp.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, origin, attempts, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answer = %v, want A from fallback", resp.Answer)
	}
	if origin != fallback.URL {
		t.Fatalf("origin = %q, want fallback %q", origin, fallback.URL)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 1 {
		t.Fatalf("hits primary/fallback = %d/%d, want 1/1", primaryHits.Load(), fallbackHits.Load())
	}
	if len(attempts) != 2 || attempts[0].Err != "" || attempts[1].Err != "" {
		t.Fatalf("attempts = %+v", attempts)
	}
}

// TestGroupNodataEverywhereKeepsPrimary проверяет, что если и резерв ответил
// NODATA, возвращается исходный NODATA основного сервера.
func TestGroupNodataEverywhereKeepsPrimary(t *testing.T) {
	primary, _ := newDoHReplyServer(t, false)
	fallback, _ := newDoHReplyServer(t, false)

	grp, err := NewGroup("g", dohGroup(primary, fallback), time.Second)
	if err != nil {
		t.Fatalf("new group: %v", err)
	}
	defer grp.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, origin, attempts, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp == nil || resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 || len(resp.Ns) != 1 {
		t.Fatalf("resp = %+v, want primary NODATA", resp)
	}
	if origin != primary.URL {
		t.Fatalf("origin = %q, want primary %q", origin, primary.URL)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %+v, want primary+fallback", attempts)
	}
}

// TestGroupNodataWithoutFallbackReturnsImmediately проверяет, что при
// отсутствии резервных серверов NODATA возвращается без лишних попыток.
func TestGroupNodataWithoutFallbackReturnsImmediately(t *testing.T) {
	primary, hits := newDoHReplyServer(t, false)

	grp, err := NewGroup("g", dohGroup(primary, nil), time.Second)
	if err != nil {
		t.Fatalf("new group: %v", err)
	}
	defer grp.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, _, attempts, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp == nil || len(resp.Answer) != 0 {
		t.Fatalf("resp = %+v, want NODATA", resp)
	}
	if hits.Load() != 1 || len(attempts) != 1 {
		t.Fatalf("hits/attempts = %d/%d, want 1/1", hits.Load(), len(attempts))
	}
}

// TestGroupNodataNonAddressDoesNotFallBack проверяет, что NODATA для типов,
// отличных от A/AAAA (например, HTTPS/SVCB), не проверяется резервом: такой
// NODATA — штатный ответ.
func TestGroupNodataNonAddressDoesNotFallBack(t *testing.T) {
	primary, primaryHits := newDoHReplyServer(t, false)
	fallback, fallbackHits := newDoHReplyServer(t, true)

	grp, err := NewGroup("g", dohGroup(primary, fallback), time.Second)
	if err != nil {
		t.Fatalf("new group: %v", err)
	}
	defer grp.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeHTTPS)
	resp, _, attempts, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp == nil || resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 {
		t.Fatalf("resp = %+v, want NODATA", resp)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 0 || len(attempts) != 1 {
		t.Fatalf("hits primary/fallback = %d/%d, attempts = %d, want 1/0 and 1",
			primaryHits.Load(), fallbackHits.Load(), len(attempts))
	}
}

// TestGroupPositivePrimarySkipsFallback проверяет, что положительный ответ
// основного сервера не вызывает обращений к резерву.
func TestGroupPositivePrimarySkipsFallback(t *testing.T) {
	primary, _ := newDoHReplyServer(t, true)
	fallback, fallbackHits := newDoHReplyServer(t, true)

	grp, err := NewGroup("g", dohGroup(primary, fallback), time.Second)
	if err != nil {
		t.Fatalf("new group: %v", err)
	}
	defer grp.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, origin, attempts, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(resp.Answer) != 1 || origin != primary.URL {
		t.Fatalf("resp/origin = %+v/%q", resp, origin)
	}
	if fallbackHits.Load() != 0 || len(attempts) != 1 {
		t.Fatalf("fallback hits/attempts = %d/%d, want 0/1", fallbackHits.Load(), len(attempts))
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
	resp, addr, attempts, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answer = %v, want 1 record", resp.Answer)
	}
	if !strings.HasPrefix(addr, "http") {
		t.Fatalf("ответил %q, ожидался DoH-fallback", addr)
	}
	// Две попытки: неудачный primary и успешный DoH-fallback.
	if len(attempts) != 2 || attempts[0].Err == "" || attempts[1].Err != "" {
		t.Fatalf("attempts = %+v", attempts)
	}
}
