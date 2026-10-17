package upstream

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

// TestDoHStringHidesSecret проверяет, что идентификатор DoH-апстрима в
// статистике не содержит секретный путь и query-параметры.
func TestDoHStringHidesSecret(t *testing.T) {
	const secret = "l1yPpn5aumqpqGrVt8Z5SWcs7SlX-VRfuw4HJWQ8fLo"
	r, err := newDoH("https://stream2.sla9.ru:8443/"+secret+"/dns-query?key="+secret, nil, nil, time.Second)
	if err != nil {
		t.Fatalf("newDoH: %v", err)
	}
	if got, want := r.String(), "https://stream2.sla9.ru:8443"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// TestDoHErrorHidesSecret проверяет, что текст ошибки DoH не содержит
// секретный путь: иначе он попадает в статистику и логи.
func TestDoHErrorHidesSecret(t *testing.T) {
	const secret = "secret-token"
	// Порт 1 недоступен — ошибка соединения возникает сразу.
	r, err := newDoH("http://127.0.0.1:1/"+secret+"/dns-query", nil, nil, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("newDoH: %v", err)
	}
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	_, err = r.Exchange(context.Background(), q)
	if err == nil {
		t.Fatalf("ожидалась ошибка соединения")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("ошибка содержит секретный путь: %v", err)
	}
	if !strings.Contains(err.Error(), "http://127.0.0.1:1") {
		t.Fatalf("ошибка не содержит origin апстрима: %v", err)
	}
}

// TestDoHInvalidURL проверяет отказ при некорректном URL.
func TestDoHInvalidURL(t *testing.T) {
	if _, err := newDoH("dns.google/dns-query", nil, nil, time.Second); err == nil {
		t.Fatalf("ожидалась ошибка для URL без scheme и host")
	}
}
