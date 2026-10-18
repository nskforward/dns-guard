// Package upstream реализует клиентов к вышестоящим DNS-серверам
// по протоколам UDP, DoT (DNS-over-TLS) и DoH (DNS-over-HTTPS),
// а также группу с основными и резервными серверами.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
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
// Err пуст, если сервер ответил; Elapsed — длительность попытки. Rcode и
// Nodata описывают ответ сервера (для отображения цепочки в UI). Timeout
// отмечает попытку, завершившуюся по превышению времени ожидания.
type Attempt struct {
	Upstream string
	Err      string
	Elapsed  time.Duration
	Rcode    int  // код ответа сервера; 0, если попытка завершилась ошибкой
	Nodata   bool // NOERROR без записей в секции Answer
	Timeout  bool // сервер не ответил за отведённое время
}

// IsTimeout сообщает, что ошибка обращения к апстриму вызвана превышением
// времени ожидания ответа: сработал таймаут резолвера или общий бюджет запроса.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Group — группа апстримов: список основных и резервных серверов.
// Резервные используются при ошибке запроса к основным, а также когда основной
// сервер ответил проблемно: NODATA (NOERROR без записей), NXDOMAIN, SERVFAIL
// или любым другим кодом, кроме NOERROR с записями. Такой ответ не считается
// окончательным и проверяется остальными серверами группы, независимо от типа
// запроса. Фоновая проверка доступности не выполняется.
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
// резервные. Окончательным считается только успешный ответ — NOERROR с хотя бы
// одной записью в секции Answer. Любой другой ответ (NODATA, NXDOMAIN, SERVFAIL,
// REFUSED и прочие коды) от основного сервера проверяется остальными серверами
// группы, независимо от типа запроса: NODATA и NXDOMAIN могут быть подменой или
// ошибкой резолвера, а SERVFAIL и необычные коды — его собственным сбоем.
//
// Если ни один сервер не дал успешного ответа, возвращается лучший из
// полученных: определённый ответ о домене (NODATA/NXDOMAIN) предпочтительнее
// сбоя (SERVFAIL и прочие коды), а внутри одного класса выигрывает первый по
// порядку обхода — то есть ответ основного сервера.
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

	// candidate — ответ сервера, не являющийся окончательным. Кандидаты двух
	// классов: определённый ответ о домене (NODATA/NXDOMAIN) и сбой сервера
	// (SERVFAIL и прочие коды). Хранится первый ответ каждого класса.
	type candidate struct {
		resp   *dns.Msg
		origin string
	}
	var (
		lastErr  error
		definite *candidate // NODATA/NXDOMAIN — определённый ответ о домене
		failure  *candidate // SERVFAIL и прочие коды — сбой сервера
	)
	attempts := make([]Attempt, 0, len(order))
	for _, r := range order {
		attemptStart := time.Now()
		resp, err := r.Exchange(ctx, m)
		elapsed := time.Since(attemptStart)
		if err != nil {
			lastErr = err
			attempts = append(attempts, Attempt{
				Upstream: r.String(),
				Err:      err.Error(),
				Elapsed:  elapsed,
				Timeout:  IsTimeout(err),
			})
			continue
		}
		attempts = append(attempts, Attempt{
			Upstream: r.String(),
			Elapsed:  elapsed,
			Rcode:    resp.Rcode,
			Nodata:   isNoData(resp),
		})
		// Успешный ответ с записями — окончательный.
		if isGoodAnswer(resp) {
			return resp, r.String(), attempts, nil
		}
		// Проблемный ответ не отдаётся сразу: проверяем остальные серверы
		// группы в надежде получить реальные записи. Определённый негатив
		// (NODATA/NXDOMAIN) может оказаться подменой или ошибкой резолвера,
		// а SERVFAIL и необычные коды — его собственным сбоем.
		if isDefiniteNegative(resp) {
			if definite == nil {
				definite = &candidate{resp: resp, origin: r.String()}
			}
		} else if failure == nil {
			failure = &candidate{resp: resp, origin: r.String()}
		}
	}
	// Ни один сервер не дал записей: определённый ответ о домене лучше сбоя,
	// внутри класса — первый по порядку обхода (ответ основного сервера).
	if definite != nil {
		return definite.resp, definite.origin, attempts, nil
	}
	if failure != nil {
		return failure.resp, failure.origin, attempts, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all upstreams failed")
	}
	return nil, "", attempts, lastErr
}

// isGoodAnswer сообщает, что ответ содержит реальные данные: NOERROR и хотя бы
// одна запись в секции Answer. Только такой ответ считается окончательным.
func isGoodAnswer(m *dns.Msg) bool {
	return m != nil && m.Rcode == dns.RcodeSuccess && len(m.Answer) > 0
}

// isNoData сообщает, что ответ не содержит записей (NODATA): NOERROR и пустая
// секция Answer.
func isNoData(m *dns.Msg) bool {
	return m != nil && m.Rcode == dns.RcodeSuccess && len(m.Answer) == 0
}

// isDefiniteNegative сообщает, что ответ является определённым ответом о домене
// (NODATA или NXDOMAIN), а не сбоем сервера.
func isDefiniteNegative(m *dns.Msg) bool {
	return m != nil && (m.Rcode == dns.RcodeNameError || isNoData(m))
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
