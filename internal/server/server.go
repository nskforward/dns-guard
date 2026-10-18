// Package server реализует DNS-пайплайн: маршрутизацию по зонам, кэш,
// обращение к вышестоящим серверам и применение границ TTL. Один и тот же
// обработчик обслуживает UDP- и TCP-слушатели; конфигурация применяется на лету.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/nskforward/dns-guard/internal/cache"
	"github.com/nskforward/dns-guard/internal/config"
	"github.com/nskforward/dns-guard/internal/rules"
	"github.com/nskforward/dns-guard/internal/stats"
	"github.com/nskforward/dns-guard/internal/upstream"
	"golang.org/x/sync/singleflight"
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
	ecs          config.ECS      // настройки EDNS Client Subnet
	ecsOff       map[string]bool // группы с ECS, отключённым через override
	block        *rules.Blocklist
	blockMode    string // nodata | zero
}

// Server обслуживает DNS-запросы.
type Server struct {
	st    atomic.Pointer[state]
	cache *cache.Cache
	stats *stats.Stats
	log   *slog.Logger
	// sf дедуплицирует одинаковые одновременные запросы: пока первый запрос
	// ждёт апстрим, остальные ждут его результат вместо повторного обращения.
	sf singleflight.Group

	// ECS: текущие подсети из конфигурации.
	ecsMu   sync.RWMutex
	ecsNets ecsNets
}

// New собирает сервер по конфигурации.
func New(cfg *config.Config, c *cache.Cache, st *stats.Stats, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if c == nil && cfg.Cache.Enabled {
		c = cache.New(cfg.Cache.MaxEntries, cfg.Cache.MinTTL, cfg.Cache.MaxTTL, cfg.Cache.NegTTL)
	}
	s := &Server{cache: c, stats: st, log: log}
	nst, err := buildState(cfg, cfg.Timeout(), cfg.Cache.Enabled)
	if err != nil {
		return nil, err
	}
	s.st.Store(nst)
	s.applyECSConfig(cfg)
	return s, nil
}

// Reload применяет новую конфигурацию и закрывает резолверы старой.
func (s *Server) Reload(cfg *config.Config) error {
	nst, err := buildState(cfg, cfg.Timeout(), cfg.Cache.Enabled)
	if err != nil {
		return err
	}
	old := s.st.Swap(nst)
	s.applyECSConfig(cfg)
	if old != nil {
		closeState(old)
		// Ответы, полученные с прежней ECS-подсетью, для новой уже неверны.
		// Смена блоклиста — тоже: положительный ответ, закэшированный до
		// блокировки, не должен обходить её до истечения TTL.
		if s.cache != nil && (!sameECSConfig(old, nst) || !sameBlockConfig(old, nst)) {
			s.cache.Flush()
		}
	}
	return nil
}

// sameBlockConfig сообщает, одинаково ли поведение блоклиста у двух снимков.
// Смена режима при пустом списке ни на что не влияет и кэш не сбрасывает.
func sameBlockConfig(a, b *state) bool {
	if !a.block.Equal(b.block) {
		return false
	}
	return a.blockMode == b.blockMode || a.block.Len() == 0
}

// sameECSConfig сообщает, одинаковы ли настройки ECS у двух снимков.
func sameECSConfig(a, b *state) bool {
	if a.ecs != b.ecs {
		return false
	}
	if len(a.ecsOff) != len(b.ecsOff) {
		return false
	}
	for name, off := range a.ecsOff {
		if b.ecsOff[name] != off {
			return false
		}
	}
	return true
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
	ecsOff := make(map[string]bool)
	for name, g := range cfg.Groups {
		if g.ECS != nil && !*g.ECS {
			ecsOff[name] = true
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
		ecs:          cfg.ECS,
		ecsOff:       ecsOff,
		block:        rules.NewBlocklist(cfg.Block.Domains),
		blockMode:    cfg.Block.ModeOrDefault(),
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
	// Блоклист проверяется до кэша: положительный ответ, полученный и
	// закэшированный до блокировки, не должен её обходить. Смена блоклиста на
	// лету дополнительно сбрасывает кэш (см. Reload).
	if entry, ok := st.block.Match(q.Name); ok {
		s.record(q.Name, "", false, blockedInfo(q.Qtype, entry))
		writeReply(w, r, blockedReply(r, q, st.blockMode))
		return
	}
	group, groupName := st.pick(q.Name)
	do := hasDO(r)

	if c := s.cache; c != nil && st.cacheEnabled {
		if msg, origin, ok := c.Get(q, do); ok {
			msg.Id = r.Id
			msg.Question = r.Question
			fitEDNS(msg, r)
			s.record(q.Name, groupName, true, answerInfo(msg, q.Qtype, origin, true, 0, nil))
			writeReply(w, r, msg)
			return
		}
	}

	// Одинаковые одновременные промахи (например, ретраи клиента, параллельные
	// A/AAAA-запросы нескольких клиентов) обслуживает один поход к апстриму.
	v, _, _ := s.sf.Do(cache.Key(q, do), func() (any, error) {
		return s.exchange(st, q, do, r, group, groupName), nil
	})
	res := v.(*exchangeResult)

	if res.err != nil {
		s.log.Warn("upstream failed", "name", q.Name, "group", groupName, "err", res.err)
		s.record(q.Name, groupName, false, errorInfo(q.Qtype, res.err.Error(), res.elapsed, res.attempts))
		answer(w, r, dns.RcodeServerFailure)
		return
	}

	// Каждому ожидающему — своя копия: Id и вопрос клиента, без гонок при
	// последующей обрезке под размер UDP-ответа.
	out := res.resp.Copy()
	out.Id = r.Id
	out.Question = r.Question
	fitEDNS(out, r)
	s.record(q.Name, groupName, false, answerInfo(out, q.Qtype, res.origin, false, res.elapsed, res.attempts))
	writeReply(w, r, out)
}

// exchangeResult — результат одного обращения к апстриму, общий для всех
// запросов, ожидающих тот же ответ (singleflight).
type exchangeResult struct {
	resp     *dns.Msg
	origin   string
	elapsed  time.Duration
	attempts []upstream.Attempt
	err      error
}

// exchange выполняет запрос к группе апстримов и сохраняет успешный ответ в
// кэш. Возвращает результат, который далее раздаётся всем ожидающим запросам.
func (s *Server) exchange(st *state, q dns.Question, do bool, r *dns.Msg, group exchanger, groupName string) *exchangeResult {
	ctx, cancel := context.WithTimeout(context.Background(), st.budget)
	defer cancel()

	up := r.Copy()
	up.RecursionDesired = true
	// Свой Id для запроса к апстриму — гигиена против коллизий с Id клиента.
	up.Id = uint16(rand.Uint32())
	// EDNS Client Subnet: сообщаем апстриму нашу публичную подсеть, чтобы
	// гео-зависимые CDN вернули адреса, близкие к нам. Для групп с явным
	// override ecs: false подсеть не передаётся.
	if st.ecs.Enabled && !st.ecsOff[groupName] {
		s.applyECS(up, q.Qtype)
	}

	start := time.Now()
	resp, origin, attempts, err := group.Exchange(ctx, up)
	elapsed := time.Since(start)
	s.recordAttempts(groupName, attempts)
	if err == nil && resp == nil {
		err = errors.New("empty response from upstream")
	}
	if err != nil {
		return &exchangeResult{elapsed: elapsed, attempts: attempts, err: err}
	}

	applyTTL(resp, st.minTTL, st.maxTTL)
	if c := s.cache; c != nil && st.cacheEnabled {
		c.Put(q, do, resp, origin)
	}
	return &exchangeResult{resp: resp, origin: origin, elapsed: elapsed, attempts: attempts}
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
		s.stats.RecordUpstream(group, a.Upstream, a.Err != "", a.Elapsed)
	}
}

// answerInfo описывает выданный ответ для страницы статистики.
func answerInfo(m *dns.Msg, qtype uint16, upstream string, fromCache bool, elapsed time.Duration, attempts []upstream.Attempt) *stats.AnswerInfo {
	info := &stats.AnswerInfo{
		Rcode:      rcodeName(m.Rcode),
		Qtype:      typeName(qtype),
		Upstream:   upstream,
		FromCache:  fromCache,
		DurationMs: float64(elapsed) / float64(time.Millisecond),
		Attempts:   attemptInfos(attempts),
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
func errorInfo(qtype uint16, msg string, elapsed time.Duration, attempts []upstream.Attempt) *stats.AnswerInfo {
	return &stats.AnswerInfo{
		Rcode:      "SERVFAIL",
		Qtype:      typeName(qtype),
		Error:      msg,
		DurationMs: float64(elapsed) / float64(time.Millisecond),
		Attempts:   attemptInfos(attempts),
	}
}

// blockTTL — TTL фиктивных записей заблокированного домена.
const blockTTL = 60

// blockedReply формирует ответ для заблокированного домена: пустой NOERROR
// (режим nodata) либо A/AAAA с адресом 0.0.0.0 / :: (режим zero), по которому
// невозможно установить соединение. Для остальных типов запросов в обоих
// режимах возвращается пустой NOERROR.
func blockedReply(r *dns.Msg, q dns.Question, mode string) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	if opt := r.IsEdns0(); opt != nil {
		m.SetEdns0(opt.UDPSize(), opt.Do())
	}
	if mode != config.BlockModeZero {
		return m
	}
	name := dns.Fqdn(q.Name)
	var rec string
	switch q.Qtype {
	case dns.TypeA:
		rec = fmt.Sprintf("%s %d IN A 0.0.0.0", name, blockTTL)
	case dns.TypeAAAA:
		rec = fmt.Sprintf("%s %d IN AAAA ::", name, blockTTL)
	default:
		return m
	}
	if rr, err := dns.NewRR(rec); err == nil {
		m.Answer = append(m.Answer, rr)
	}
	return m
}

// blockedInfo описывает заблокированный ответ для статистики.
func blockedInfo(qtype uint16, entry string) *stats.AnswerInfo {
	return &stats.AnswerInfo{
		Rcode:     "NOERROR",
		Qtype:     typeName(qtype),
		Blocked:   true,
		BlockedBy: entry,
	}
}

// attemptInfos конвертирует попытки обращения к апстримам в сведения для
// статистики: адрес сервера, итог попытки и длительность.
func attemptInfos(attempts []upstream.Attempt) []stats.AttemptInfo {
	if len(attempts) == 0 {
		return nil
	}
	out := make([]stats.AttemptInfo, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, stats.AttemptInfo{
			Upstream:   a.Upstream,
			Status:     attemptStatus(a),
			Error:      a.Err,
			DurationMs: float64(a.Elapsed) / float64(time.Millisecond),
		})
	}
	return out
}

// attemptStatus возвращает человекочитаемый итог одной попытки.
func attemptStatus(a upstream.Attempt) string {
	if a.Err != "" {
		return "ERROR"
	}
	if a.Nodata {
		return "NODATA"
	}
	if a.Rcode == dns.RcodeSuccess {
		return "OK"
	}
	return rcodeName(a.Rcode)
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
