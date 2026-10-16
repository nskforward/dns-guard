package upstream

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// newDoHTestServer поднимает тестовый DoH-сервер, отвечающий на любой запрос
// фиксированной A-записью 192.0.2.1.
func newDoHTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if err := q.Unpack(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := new(dns.Msg)
		resp.SetReply(q)
		resp.Answer = append(resp.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.IPv4(192, 0, 2, 1),
		})
		out, _ := resp.Pack()
		w.Header().Set("Content-Type", dohContentType)
		_, _ = w.Write(out)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestDoHStaticHost проверяет, что hosts позволяет достучаться до DoH-апстрима
// по имени, которое не резолвится системным DNS: соединение идёт на статический IP,
// а хостнейм из URL при этом сохраняется.
func TestDoHStaticHost(t *testing.T) {
	srv := newDoHTestServer(t)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse httptest url: %v", err)
	}
	_, port, _ := net.SplitHostPort(u.Host)
	// Имя в .invalid не резолвится ни одним DNS-сервером.
	dohURL := "http://doh.invalid:" + port + "/dns-query"

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)

	// Без маппинга резолв имени падает.
	r0, err := newDoH(dohURL, nil, nil, 500*time.Millisecond)
	if err == nil {
		if _, err := r0.Exchange(context.Background(), q); err == nil {
			t.Fatalf("ожидалась ошибка резолва без hosts")
		}
	}

	// С маппингом соединение уходит на 127.0.0.1.
	r, err := newDoH(dohURL, nil, map[string]string{"doh.invalid": "127.0.0.1"}, time.Second)
	if err != nil {
		t.Fatalf("newDoH: %v", err)
	}
	resp, err := r.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answer = %v, want 1 record", resp.Answer)
	}
}
