package upstream

import (
	"context"
	"errors"
	"fmt"
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

// newDoHRcodeServer поднимает DoH-сервер, отвечающий заданным rcode без
// записей, как это делает «сломавшийся» рекурсор. Возвращает сервер и счётчик
// полученных запросов.
func newDoHRcodeServer(t *testing.T, rcode int) (*httptest.Server, *atomic.Int32) {
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
		resp.Rcode = rcode
		out, _ := resp.Pack()
		w.Header().Set("Content-Type", dohContentType)
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newDoHServfailServer поднимает DoH-сервер, отвечающий rcode=SERVFAIL,
// как это делает «сломавшийся» рекурсор. Возвращает сервер и счётчик
// полученных запросов.
func newDoHServfailServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	return newDoHRcodeServer(t, dns.RcodeServerFailure)
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
	// Первая попытка — NODATA основного, вторая — успешный ответ резерва.
	if !attempts[0].Nodata || attempts[0].Rcode != dns.RcodeSuccess ||
		attempts[1].Nodata || attempts[1].Rcode != dns.RcodeSuccess {
		t.Fatalf("attempts = %+v", attempts)
	}
}

// TestGroupNodataEverywhereKeepsLast проверяет, что если и резерв ответил
// NODATA, возвращается NODATA последнего сервера цепочки: в карточке и кэше
// видно, что fallback отработал и подтвердил ответ.
func TestGroupNodataEverywhereKeepsLast(t *testing.T) {
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
		t.Fatalf("resp = %+v, want NODATA", resp)
	}
	if origin != fallback.URL {
		t.Fatalf("origin = %q, want fallback %q", origin, fallback.URL)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %+v, want primary+fallback", attempts)
	}
	if !attempts[0].Nodata || !attempts[1].Nodata {
		t.Fatalf("attempts = %+v, want NODATA on both", attempts)
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

// TestGroupNodataNonAddressFallsBack проверяет, что NODATA для типов, отличных
// от A/AAAA (например, HTTPS/SVCB), тоже проверяется резервом: такой ответ
// может быть ошибкой основного резолвера, а не ответом о домене.
func TestGroupNodataNonAddressFallsBack(t *testing.T) {
	primary, primaryHits := newDoHReplyServer(t, false)
	fallback, fallbackHits := newDoHReplyServer(t, true)

	grp, err := NewGroup("g", dohGroup(primary, fallback), time.Second)
	if err != nil {
		t.Fatalf("new group: %v", err)
	}
	defer grp.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeHTTPS)
	resp, origin, attempts, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp == nil || resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("resp = %+v, want A from fallback", resp)
	}
	if origin != fallback.URL {
		t.Fatalf("origin = %q, want fallback %q", origin, fallback.URL)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 1 || len(attempts) != 2 {
		t.Fatalf("hits primary/fallback = %d/%d, attempts = %d, want 1/1 and 2",
			primaryHits.Load(), fallbackHits.Load(), len(attempts))
	}
	if !attempts[0].Nodata || attempts[1].Nodata {
		t.Fatalf("attempts = %+v", attempts)
	}
}

// TestGroupNxdomainFromPrimaryFallsBack проверяет, что NXDOMAIN основного
// сервера не отдаётся клиенту сразу: положительный ответ резерва выигрывает.
func TestGroupNxdomainFromPrimaryFallsBack(t *testing.T) {
	primary, primaryHits := newDoHRcodeServer(t, dns.RcodeNameError)
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
	if len(resp.Answer) != 1 || resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("resp = %+v, want A from fallback", resp)
	}
	if origin != fallback.URL {
		t.Fatalf("origin = %q, want fallback %q", origin, fallback.URL)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 1 || len(attempts) != 2 {
		t.Fatalf("hits primary/fallback = %d/%d, attempts = %d, want 1/1 and 2",
			primaryHits.Load(), fallbackHits.Load(), len(attempts))
	}
	if attempts[0].Rcode != dns.RcodeNameError || attempts[1].Rcode != dns.RcodeSuccess {
		t.Fatalf("attempts = %+v, want NXDOMAIN then NOERROR", attempts)
	}
}

// TestGroupNxdomainEverywhereKeepsLast проверяет, что если все серверы
// ответили NXDOMAIN, клиенту возвращается NXDOMAIN последнего сервера цепочки.
func TestGroupNxdomainEverywhereKeepsLast(t *testing.T) {
	primary, _ := newDoHRcodeServer(t, dns.RcodeNameError)
	fallback, _ := newDoHRcodeServer(t, dns.RcodeNameError)

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
	if resp == nil || resp.Rcode != dns.RcodeNameError {
		t.Fatalf("resp = %+v, want NXDOMAIN", resp)
	}
	if origin != fallback.URL {
		t.Fatalf("origin = %q, want fallback %q", origin, fallback.URL)
	}
	if len(attempts) != 2 ||
		attempts[0].Rcode != dns.RcodeNameError || attempts[1].Rcode != dns.RcodeNameError {
		t.Fatalf("attempts = %+v, want NXDOMAIN on both", attempts)
	}
}

// TestGroupMixedNegativeKeepsPrimary проверяет, что негативные ответы с разными
// кодами не подменяют друг друга: NODATA основного остаётся итоговым, даже
// если резерв ответил NXDOMAIN.
func TestGroupMixedNegativeKeepsPrimary(t *testing.T) {
	primary, _ := newDoHReplyServer(t, false)
	fallback, _ := newDoHRcodeServer(t, dns.RcodeNameError)

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
	if resp == nil || resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 {
		t.Fatalf("resp = %+v, want primary NODATA", resp)
	}
	if origin != primary.URL {
		t.Fatalf("origin = %q, want primary %q", origin, primary.URL)
	}
	if len(attempts) != 2 || !attempts[0].Nodata || attempts[1].Rcode != dns.RcodeNameError {
		t.Fatalf("attempts = %+v", attempts)
	}
}

// TestGroupRefusedFromPrimaryFallsBack проверяет, что любой необычный код
// ответа основного сервера (например, REFUSED) проверяется резервом.
func TestGroupRefusedFromPrimaryFallsBack(t *testing.T) {
	primary, _ := newDoHRcodeServer(t, dns.RcodeRefused)
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
	if len(resp.Answer) != 1 || origin != fallback.URL {
		t.Fatalf("resp/origin = %+v/%q, want fallback answer", resp, origin)
	}
	if fallbackHits.Load() != 1 || len(attempts) != 2 {
		t.Fatalf("fallback hits/attempts = %d/%d, want 1/2", fallbackHits.Load(), len(attempts))
	}
	if attempts[0].Rcode != dns.RcodeRefused {
		t.Fatalf("attempts = %+v, want REFUSED first", attempts)
	}
}

// TestGroupNxdomainPrimaryNodataFallback проверяет приоритет определённых
// ответов: NXDOMAIN/NODATA резерва предпочтительнее сбоя основного, но при
// двух определённых негативах остаётся ответ основного.
func TestGroupNxdomainPrimaryNodataFallback(t *testing.T) {
	primary, _ := newDoHRcodeServer(t, dns.RcodeNameError)
	fallback, _ := newDoHReplyServer(t, false)

	grp, err := NewGroup("g", dohGroup(primary, fallback), time.Second)
	if err != nil {
		t.Fatalf("new group: %v", err)
	}
	defer grp.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, origin, _, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp == nil || resp.Rcode != dns.RcodeNameError || origin != primary.URL {
		t.Fatalf("resp/origin = %+v/%q, want primary NXDOMAIN", resp, origin)
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

// TestGroupServfailFromPrimaryFallsBack проверяет, что SERVFAIL основного
// сервера не отдаётся клиенту сразу, а проверяется резервом: положительный
// ответ резерва выигрывает.
func TestGroupServfailFromPrimaryFallsBack(t *testing.T) {
	primary, primaryHits := newDoHServfailServer(t)
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
	if len(resp.Answer) != 1 || resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("resp = %+v, want A from fallback", resp)
	}
	if origin != fallback.URL {
		t.Fatalf("origin = %q, want fallback %q", origin, fallback.URL)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 1 {
		t.Fatalf("hits primary/fallback = %d/%d, want 1/1", primaryHits.Load(), fallbackHits.Load())
	}
	// Первая попытка — SERVFAIL основного, вторая — успешный ответ резерва.
	if len(attempts) != 2 || attempts[0].Err != "" || attempts[1].Err != "" {
		t.Fatalf("attempts = %+v", attempts)
	}
	if attempts[0].Rcode != dns.RcodeServerFailure || attempts[1].Rcode != dns.RcodeSuccess {
		t.Fatalf("attempts = %+v, want SERVFAIL then NOERROR", attempts)
	}
}

// TestGroupServfailNonAddressFallsBack проверяет, что в отличие от NODATA
// SERVFAIL основного проверяется резервом для любых типов запроса, а не
// только A/AAAA: SERVFAIL никогда не является штатным ответом.
func TestGroupServfailNonAddressFallsBack(t *testing.T) {
	primary, _ := newDoHServfailServer(t)
	fallback, fallbackHits := newDoHReplyServer(t, true)

	grp, err := NewGroup("g", dohGroup(primary, fallback), time.Second)
	if err != nil {
		t.Fatalf("new group: %v", err)
	}
	defer grp.Close()

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeHTTPS)
	resp, origin, attempts, err := grp.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(resp.Answer) != 1 || origin != fallback.URL {
		t.Fatalf("resp/origin = %+v/%q, want fallback answer", resp, origin)
	}
	if fallbackHits.Load() != 1 || len(attempts) != 2 {
		t.Fatalf("fallback hits/attempts = %d/%d, want 1/2", fallbackHits.Load(), len(attempts))
	}
}

// TestGroupServfailEverywhereKeepsLast проверяет, что если и резерв ответил
// SERVFAIL, клиенту возвращается SERVFAIL последнего сервера цепочки, а не
// ошибка канала.
func TestGroupServfailEverywhereKeepsLast(t *testing.T) {
	primary, _ := newDoHServfailServer(t)
	fallback, _ := newDoHServfailServer(t)

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
	if resp == nil || resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("resp = %+v, want SERVFAIL", resp)
	}
	if origin != fallback.URL {
		t.Fatalf("origin = %q, want fallback %q", origin, fallback.URL)
	}
	if len(attempts) != 2 ||
		attempts[0].Rcode != dns.RcodeServerFailure || attempts[1].Rcode != dns.RcodeServerFailure {
		t.Fatalf("attempts = %+v, want SERVFAIL on both", attempts)
	}
}

// TestGroupServfailPrimaryNodataFallback проверяет, что NODATA резерва при
// SERVFAIL основного возвращается как валидный ответ: определённый негативный
// ответ лучше, чем сбой резолвера.
func TestGroupServfailPrimaryNodataFallback(t *testing.T) {
	primary, _ := newDoHServfailServer(t)
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
		t.Fatalf("resp = %+v, want fallback NODATA", resp)
	}
	if origin != fallback.URL {
		t.Fatalf("origin = %q, want fallback %q", origin, fallback.URL)
	}
	if len(attempts) != 2 || attempts[0].Rcode != dns.RcodeServerFailure || !attempts[1].Nodata {
		t.Fatalf("attempts = %+v", attempts)
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

// TestGroupTimeoutFromPrimaryFallsBack проверяет, что таймаут основного
// сервера не отдаётся клиенту: попытка помечается Timeout, а запрос уходит
// следующим серверам группы, включая резерв.
func TestGroupTimeoutFromPrimaryFallsBack(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		http.Error(w, "too late", http.StatusGatewayTimeout)
	}))
	t.Cleanup(slow.Close)
	fallback, fallbackHits := newDoHReplyServer(t, true)

	g := config.Group{
		Protocol: "doh",
		Primary:  []string{slow.URL + "/dns-query"},
		Fallback: []string{fallback.URL + "/dns-query"},
	}
	grp, err := NewGroup("g", g, 100*time.Millisecond)
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
	if resp == nil || len(resp.Answer) != 1 || origin != fallback.URL {
		t.Fatalf("resp/origin = %+v/%q, want fallback answer", resp, origin)
	}
	if fallbackHits.Load() != 1 || len(attempts) != 2 {
		t.Fatalf("fallback hits/attempts = %d/%d, want 1/2", fallbackHits.Load(), len(attempts))
	}
	if !attempts[0].Timeout || attempts[0].Err == "" {
		t.Fatalf("attempt[0] = %+v, want timeout with error text", attempts[0])
	}
	if attempts[1].Timeout || attempts[1].Err != "" || attempts[1].Rcode != dns.RcodeSuccess {
		t.Fatalf("attempt[1] = %+v, want success", attempts[1])
	}
}

// TestIsTimeout проверяет классификацию ошибок превышения времени ожидания.
func TestIsTimeout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("doh: %w", context.DeadlineExceeded), true},
		{"net timeout", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, true},
		{"refused", &net.DNSError{Err: "connection refused"}, false},
		{"plain", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := IsTimeout(tc.err); got != tc.want {
			t.Errorf("%s: IsTimeout = %v, want %v", tc.name, got, tc.want)
		}
	}
}
