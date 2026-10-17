// Package server реализует DNS-пайплайн: маршрутизацию по зонам, кэш,
// обращение к вышестоящим серверам и применение границ TTL. Один и тот же
// обработчик обслуживает UDP- и TCP-слушатели; конфигурация применяется на лету.
package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/nskforward/dns-guard/internal/cache"
	"github.com/nskforward/dns-guard/internal/config"
	"github.com/nskforward/dns-guard/internal/rules"
	"github.com/nskforward/dns-guard/internal/stats"
	"github.com/nskforward/dns-guard/internal/upstream"
)

// exchanger — вышестоящая группа, способная обменяться сообщением.
// Позволяет подменять upstream.Group в тестах.
type exchanger interface {
	Exchange(ctx context.Context, m *dns.Msg) (*dns.Msg, string, []upstream.Attempt, error)
	io.Closer
}

// state — неизменяемый снимок конфигурации, подменяемый целиком при reload.
type state struct {
	matcher      *rules.Matcher
	groups       map[string]exchanger
	def          exchanger
	defName      string
	budget       time.Duration // общий бюджет запроса: per-attempt timeout × макс. попыток группы
	cacheEnabled bool
	minTTL       uint32
	maxTTL       uint32
}

// Server обслуживает DNS-запросы.
type Server struct {
	st    atomic.Pointer[state]
	cache *cache.Cache
	stats *stats.Stats
	log   *slog.Logger
}

// New собирает сервер по конфигурации.
func New(cfg *config.Config, c *cache.Cache, st *stats.Stats, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if c == nil && cfg.Cache.Enabled {
		c = cache.New(cfg.Cache.MaxEntries, cfg.Cache.MinTTL, cfg.Cache.MaxTTL)
	}
	s := &Server{cache: c, stats: st, log: log}
	nst, err := buildState(cfg, cfg.Timeout(), cfg.Cache.Enabled)
	if err != nil {
		return nil, err
	}
	s.st.Store(nst)
	return s, nil
}

// Reload применяет новую конфигурацию и закрывает резолверы старой.
func (s *Server) Reload(cfg *config.Config) error {
	nst, err := buildState(cfg, cfg.Timeout(), cfg.Cache.Enabled)
	if err != nil {
		return err
	}
	old := s.st.Swap(nst)
	if old != nil {
		closeState(old)
	}
	return nil
}

func buildState(cfg *config.Config, timeout time.Duration, cacheEnabled bool) (*state, error) {
	groups := make(map[string]exchanger, len(cfg.Groups))
	// Бюджет запроса должен вмещать все попытки группы (primary + fallback),
	// иначе первая же «висящая» попытка съест весь бюджет и fallback не сработает.
	maxAttempts := 1
	for name, g := range cfg.Groups {
		grp, err := upstream.NewGroup(name, g, timeout)
		if err != nil {
			for _, built := range groups {
				_ = built.Close()
			}
			return nil, err
		}
		groups[name] = grp
		if n := len(g.Primary) + len(g.Fallback); n > maxAttempts {
			maxAttempts = n
		}
	}
	return &state{
		matcher:      rules.New(cfg.Rules),
		groups:       groups,
		def:          groups[cfg.DefaultGroup],
		defName:      cfg.DefaultGroup,
		budget:       time.Duration(maxAttempts) * timeout,
		cacheEnabled: cacheEnabled,
		minTTL:       cfg.Cache.MinTTL,
		maxTTL:       cfg.Cache.MaxTTL,
	}, nil
}

func closeState(st *state) {
	for _, g := range st.groups {
		_ = g.Close()
	}
}

// ServeDNS обрабатывает один запрос (общий код для UDP и TCP).
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	st := s.st.Load()
	if st == nil {
		answer(w, r, dns.RcodeServerFailure)
		return
	}
	if r == nil || r.Opcode != dns.OpcodeQuery || len(r.Question) != 1 {
		answer(w, r, dns.RcodeRefused)
		return
	}

	q := r.Question[0]
	group, groupName := st.pick(q.Name)
	do := hasDO(r)

	if c := s.cache; c != nil && st.cacheEnabled {
		if msg, origin, ok := c.Get(q, do); ok {
			msg.Id = r.Id
			s.record(q.Name, groupName, true, answerInfo(msg, q.Qtype, origin, true))
			writeReply(w, r, msg)
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), st.budget)
	defer cancel()

	up := r.Copy()
	up.RecursionDesired = true
	resp, origin, attempts, err := group.Exchange(ctx, up)
	s.recordAttempts(groupName, attempts)
	if err != nil {
		s.log.Warn("upstream failed", "name", q.Name, "group", groupName, "err", err)
		s.record(q.Name, groupName, false, errorInfo(q.Qtype, err.Error()))
		answer(w, r, dns.RcodeServerFailure)
		return
	}
	if resp == nil {
		s.record(q.Name, groupName, false, errorInfo(q.Qtype, ""))
		answer(w, r, dns.RcodeServerFailure)
		return
	}

	resp.Id = r.Id
	if len(resp.Question) == 0 {
		resp.Question = r.Question
	}
	applyTTL(resp, st.minTTL, st.maxTTL)
	if c := s.cache; c != nil && st.cacheEnabled {
		c.Put(q, resp, origin)
	}
	s.record(q.Name, groupName, false, answerInfo(resp, q.Qtype, origin, false))
	writeReply(w, r, resp)
}

func (st *state) pick(qname string) (exchanger, string) {
	if name, ok := st.matcher.Match(qname); ok {
		if g, ok := st.groups[name]; ok {
			return g, name
		}
	}
	return st.def, st.defName
}

func (s *Server) record(name, group string, hit bool, info *stats.AnswerInfo) {
	if s.stats == nil {
		return
	}
	s.stats.Record(name, group, hit, info)
}

// recordAttempts учитывает попытки обращения к апстримам группы.
func (s *Server) recordAttempts(group string, attempts []upstream.Attempt) {
	if s.stats == nil {
		return
	}
	for _, a := range attempts {
		s.stats.RecordUpstream(group, a.Upstream, a.Err != "")
	}
}

// answerInfo описывает выданный ответ для страницы статистики.
func answerInfo(m *dns.Msg, qtype uint16, upstream string, fromCache bool) *stats.AnswerInfo {
	info := &stats.AnswerInfo{
		Rcode:     rcodeName(m.Rcode),
		Qtype:     typeName(qtype),
		Upstream:  upstream,
		FromCache: fromCache,
	}
	for _, rr := range m.Answer {
		if _, ok := rr.(*dns.OPT); ok {
			continue
		}
		info.Records = append(info.Records, rr.String())
	}
	for _, rr := range m.Ns {
		if _, ok := rr.(*dns.OPT); ok {
			continue
		}
		info.Authority = append(info.Authority, rr.String())
	}
	return info
}

// errorInfo описывает ответ, сформированный при ошибке апстрима.
func errorInfo(qtype uint16, msg string) *stats.AnswerInfo {
	return &stats.AnswerInfo{
		Rcode: "SERVFAIL",
		Qtype: typeName(qtype),
		Error: msg,
	}
}

func rcodeName(rcode int) string {
	if name, ok := dns.RcodeToString[rcode]; ok {
		return name
	}
	return fmt.Sprintf("RCODE%d", rcode)
}

func typeName(t uint16) string {
	if name, ok := dns.TypeToString[t]; ok {
		return name
	}
	return fmt.Sprintf("TYPE%d", t)
}

// writeReply отправляет ответ, обрезая его для UDP по размеру буфера клиента.
func writeReply(w dns.ResponseWriter, r, resp *dns.Msg) {
	if _, ok := w.RemoteAddr().(*net.UDPAddr); ok {
		resp.Truncate(udpSize(r))
	}
	if err := w.WriteMsg(resp); err != nil {
		slog.Debug("write reply failed", "err", err)
	}
}

// answer формирует простой ответ с заданным rcode.
func answer(w dns.ResponseWriter, r *dns.Msg, rcode int) {
	m := new(dns.Msg)
	if r != nil {
		m.SetReply(r)
		if opt := r.IsEdns0(); opt != nil {
			m.SetEdns0(opt.UDPSize(), opt.Do())
		}
	} else {
		m.Response = true
		m.Opcode = dns.OpcodeQuery
	}
	m.Rcode = rcode
	if err := w.WriteMsg(m); err != nil {
		slog.Debug("write answer failed", "err", err)
	}
}

// udpSize возвращает допустимый размер UDP-ответа с учётом EDNS(0).
func udpSize(r *dns.Msg) int {
	size := dns.MinMsgSize
	if opt := r.IsEdns0(); opt != nil {
		if s := int(opt.UDPSize()); s > size {
			size = s
		}
	}
	return size
}

func hasDO(m *dns.Msg) bool {
	if opt := m.IsEdns0(); opt != nil {
		return opt.Do()
	}
	return false
}

func applyTTL(m *dns.Msg, min, max uint32) {
	if min == 0 && max == 0 {
		return
	}
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			if _, ok := rr.(*dns.OPT); ok {
				continue
			}
			h := rr.Header()
			if min > 0 && h.Ttl < min {
				h.Ttl = min
			}
			if max > 0 && h.Ttl > max {
				h.Ttl = max
			}
		}
	}
}
