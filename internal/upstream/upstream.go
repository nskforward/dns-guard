// Package upstream реализует клиентов к вышестоящим DNS-серверам
// по протоколам UDP, DoT (DNS-over-TLS) и DoH (DNS-over-HTTPS),
// а также группу с основными и резервными серверами.
package upstream

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/nskforward/dns-guard/internal/config"
)

// Resolver — единый интерфейс вышестоящего DNS-сервера.
type Resolver interface {
	Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, error)
	String() string
	// Close освобождает ресурсы резолвера (например, idle-соединения DoH).
	Close() error
}

// New создаёт Resolver по протоколу и адресу.
//   - udp: "8.8.8.8" или "8.8.8.8:53"
//   - dot: "dns.google" или "dns.google:853"
//   - doh: "https://dns.google/dns-query"
//
// Адрес может нести необязательный префикс протокола ("udp+192.168.1.1:53",
// "doh+https://9.9.9.9/dns-query"), который перекрывает аргумент proto — это
// позволяет смешивать протоколы primary/fallback в одной группе.
//
// hosts — статический маппинг хостнейм→IP (используется вместо резолва для DoH/DoT).
func New(proto, addr string, bootstrap []string, hosts map[string]string, timeout time.Duration) (Resolver, error) {
	if p, rest, err := config.SplitProtocol(addr); err != nil {
		return nil, err
	} else if p != "" {
		proto, addr = p, rest
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	switch proto {
	case "udp":
		return newUDP(normalizeHostPort(addr, 53), timeout), nil
	case "dot":
		return newDoT(normalizeHostPort(addr, 853), bootstrap, hosts, timeout)
	case "doh":
		return newDoH(addr, bootstrap, hosts, timeout)
	default:
		return nil, fmt.Errorf("unknown protocol %q", proto)
	}
}

// Attempt — одна попытка обращения к апстриму группы.
// Err пуст, если сервер ответил успешно; Elapsed — длительность попытки.
type Attempt struct {
	Upstream string
	Err      string
	Elapsed  time.Duration
}

// Group — группа апстримов: список основных и резервных серверов.
// Резервные используются при ошибке запроса к основным, а также когда основной
// сервер ответил NODATA (NOERROR без записей) на запрос A/AAAA — такой ответ
// проверяется резервными. Для остальных типов NODATA считается штатным.
// Фоновая проверка доступности не выполняется.
type Group struct {
	Name     string
	primary  []Resolver
	fallback []Resolver
	rr       atomic.Uint64
	timeout  time.Duration
}

// NewGroup строит группу по конфигурации.
func NewGroup(name string, g config.Group, timeout time.Duration) (*Group, error) {
	grp := &Group{Name: name, timeout: timeout}
	for _, a := range g.Primary {
		r, err := New(g.Protocol, a, g.Bootstrap, g.Hosts, timeout)
		if err != nil {
			return nil, fmt.Errorf("group %q primary %q: %w", name, a, err)
		}
		grp.primary = append(grp.primary, r)
	}
	for _, a := range g.Fallback {
		r, err := New(g.Protocol, a, g.Bootstrap, g.Hosts, timeout)
		if err != nil {
			return nil, fmt.Errorf("group %q fallback %q: %w", name, a, err)
		}
		grp.fallback = append(grp.fallback, r)
	}
	if len(grp.primary) == 0 {
		grp.primary = grp.fallback
		grp.fallback = nil
	}
	if len(grp.primary) == 0 {
		return nil, fmt.Errorf("group %q: no resolvers", name)
	}
	return grp, nil
}

// Exchange отправляет запрос: сначала основные серверы (в ротации), затем
// резервные. Резервные используются при ошибке запроса, а также если основной
// сервер вернул NODATA (NOERROR без записей в Answer) на запрос A/AAAA: такой
// ответ не принимается сразу, а проверяется резервными серверами. Для прочих
// типов NODATA возвращается как есть. Если все ответившие серверы дали NODATA,
// возвращается исходный ответ основного сервера.
//
// Возвращает ответ, адрес ответившего сервера и список всех выполненных попыток
// (в порядке обхода).
func (g *Group) Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, string, []Attempt, error) {
	servers := append(append([]Resolver{}, g.primary...), g.fallback...)
	if len(servers) == 0 {
		return nil, "", nil, fmt.Errorf("no resolvers")
	}
	start := 0
	if n := len(g.primary); n > 0 {
		start = int(g.rr.Add(1)-1) % n
	}
	// порядок: основной (с ротацией) → остальные основные → резервные
	order := make([]Resolver, 0, len(servers))
	for i := 0; i < len(g.primary); i++ {
		order = append(order, g.primary[(start+i)%len(g.primary)])
	}
	order = append(order, g.fallback...)

	var (
		lastErr      error
		nodata       *dns.Msg // первый NODATA от основного сервера
		nodataOrigin string
	)
	attempts := make([]Attempt, 0, len(order))
	for i, r := range order {
		isFallback := i >= len(g.primary)
		attemptStart := time.Now()
		resp, err := r.Exchange(ctx, m)
		elapsed := time.Since(attemptStart)
		if err != nil {
			lastErr = err
			attempts = append(attempts, Attempt{Upstream: r.String(), Err: err.Error(), Elapsed: elapsed})
			continue
		}
		attempts = append(attempts, Attempt{Upstream: r.String(), Elapsed: elapsed})
		if nodata == nil && isNoData(resp) && !isFallback && len(g.fallback) > 0 && shouldVerifyNoData(m) {
			// NODATA от основного сервера может быть его ошибкой — проверяем
			// резервными (и остальными основными до них). Проверяем только
			// A/AAAA: для остальных типов NODATA — обычное дело.
			nodata, nodataOrigin = resp, r.String()
			continue
		}
		if nodata != nil && isNoData(resp) {
			// Проверяющий сервер тоже ответил NODATA — ищем дальше.
			continue
		}
		return resp, r.String(), attempts, nil
	}
	if nodata != nil {
		return nodata, nodataOrigin, attempts, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all upstreams failed")
	}
	return nil, "", attempts, lastErr
}

// isNoData сообщает, что ответ не содержит записей (NODATA): NOERROR и пустая
// секция Answer.
func isNoData(m *dns.Msg) bool {
	return m != nil && m.Rcode == dns.RcodeSuccess && len(m.Answer) == 0
}

// shouldVerifyNoData сообщает, нужно ли проверять NODATA резервными серверами.
// Проверка выполняется только для адресных запросов A/AAAA: для остальных типов
// (например, HTTPS/SVCB) NODATA — штатный ответ, и лишние обращения к резерву
// только добавляли бы задержку.
func shouldVerifyNoData(m *dns.Msg) bool {
	if len(m.Question) != 1 {
		return false
	}
	switch m.Question[0].Qtype {
	case dns.TypeA, dns.TypeAAAA:
		return true
	default:
		return false
	}
}

// Close закрывает все резолверы группы.
func (g *Group) Close() error {
	var first error
	for _, r := range append(append([]Resolver{}, g.primary...), g.fallback...) {
		if err := r.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
