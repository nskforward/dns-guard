// Package cache реализует in-memory кэш DNS-ответов с ограничением TTL снизу
// (MIN-TTL) и опционально сверху. Кэш сбрасывается при перезапуске сервиса.
package cache

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

type entry struct {
	key      string
	msg      *dns.Msg
	upstream string
	stored   time.Time
	expires  time.Time
	elem     *list.Element
}

// Cache — потокобезопасный LRU-кэш DNS-сообщений.
type Cache struct {
	mu      sync.Mutex
	max     int
	minTTL  uint32
	maxTTL  uint32
	negTTL  uint32
	now     func() time.Time
	entries map[string]*entry
	lru     *list.List
}

// New создаёт кэш. minTTL/maxTTL — границы TTL в секундах (maxTTL=0 — без
// верхней границы), negTTL — потолок TTL негативных ответов (0 — без потолка).
func New(maxEntries int, minTTL, maxTTL, negTTL uint32) *Cache {
	return newWithClock(maxEntries, minTTL, maxTTL, negTTL, time.Now)
}

func newWithClock(maxEntries int, minTTL, maxTTL, negTTL uint32, now func() time.Time) *Cache {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	return &Cache{
		max:     maxEntries,
		minTTL:  minTTL,
		maxTTL:  maxTTL,
		negTTL:  negTTL,
		now:     now,
		entries: make(map[string]*entry),
		lru:     list.New(),
	}
}

// MinTTL возвращает настроенный минимальный TTL.
func (c *Cache) MinTTL() uint32 { return c.minTTL }

// Key формирует ключ кэша по вопросу клиента. Экспортирован, чтобы вызывающий
// код мог дедуплицировать одинаковые одновременные запросы (singleflight).
func Key(q dns.Question, do bool) string {
	doBit := "0"
	if do {
		doBit = "1"
	}
	return fmt.Sprintf("%s|%d|%d|%s", strings.ToLower(q.Name), q.Qtype, q.Qclass, doBit)
}

// Get возвращает копию закэшированного ответа с уменьшенными TTL и адрес
// апстрима, от которого ответ был получен изначально (пустая строка, если не
// известен).
func (c *Cache) Get(q dns.Question, do bool) (*dns.Msg, string, bool) {
	k := Key(q, do)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return nil, "", false
	}
	now := c.now()
	if now.After(e.expires) {
		c.removeElem(e)
		return nil, "", false
	}
	c.lru.MoveToFront(e.elem)
	out := e.msg.Copy()
	elapsed := uint32(now.Sub(e.stored).Seconds())
	decTTL(out, elapsed)
	return out, e.upstream, true
}

// Put сохраняет ответ (предварительно применив границы TTL). Ключ строится по
// вопросу и DO-биту КЛИЕНТА (q, do), а не по тому, что эхоил апстрим: это
// исключает расхождение ключей Get/Put и «отравление» кэша ответом с чужим
// вопросом. upstream — адрес сервера, выдавшего ответ (может быть пустым).
func (c *Cache) Put(q dns.Question, do bool, m *dns.Msg, upstream string) {
	if m == nil || m.Truncated {
		return
	}
	if m.Rcode != dns.RcodeSuccess && m.Rcode != dns.RcodeNameError {
		return
	}
	c.Apply(m)
	ttl := minTTLOf(m)
	if ttl == 0 {
		return
	}
	if c.negTTL > 0 && isNegative(m) && ttl > c.negTTL {
		ttl = c.negTTL
	}
	k := Key(q, do)

	stored := m.Copy()
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[k]; ok {
		c.removeElem(old)
	}
	e := &entry{
		key:      k,
		msg:      stored,
		upstream: upstream,
		stored:   now,
		expires:  now.Add(time.Duration(ttl) * time.Second),
	}
	e.elem = c.lru.PushFront(e)
	c.entries[k] = e
	for c.lru.Len() > c.max {
		if back := c.lru.Back(); back != nil {
			c.removeElem(back.Value.(*entry))
		}
	}
}

// Apply применяет границы TTL (MIN-TTL / MAX-TTL) к секциям ответа.
func (c *Cache) Apply(m *dns.Msg) {
	applySection(m.Answer, c.minTTL, c.maxTTL)
	applySection(m.Ns, c.minTTL, c.maxTTL)
	applySection(m.Extra, c.minTTL, c.maxTTL)
}

// Flush очищает кэш.
func (c *Cache) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*entry)
	c.lru.Init()
}

// Len возвращает число записей в кэше.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

func (c *Cache) removeElem(e *entry) {
	if e == nil {
		return
	}
	if e.elem != nil {
		c.lru.Remove(e.elem)
	}
	delete(c.entries, e.key)
}

func applySection(rrs []dns.RR, minTTL, maxTTL uint32) {
	for _, rr := range rrs {
		if _, ok := rr.(*dns.OPT); ok {
			continue
		}
		h := rr.Header()
		if minTTL > 0 && h.Ttl < minTTL {
			h.Ttl = minTTL
		}
		if maxTTL > 0 && h.Ttl > maxTTL {
			h.Ttl = maxTTL
		}
	}
}

func decTTL(m *dns.Msg, elapsed uint32) {
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			if _, ok := rr.(*dns.OPT); ok {
				continue
			}
			h := rr.Header()
			if h.Ttl > elapsed {
				h.Ttl -= elapsed
			} else {
				h.Ttl = 0
			}
		}
	}
}

// minTTLOf вычисляет TTL хранения: минимум по ответу, иначе по SOA в authority.
func minTTLOf(m *dns.Msg) uint32 {
	var ttl uint32
	set := false
	for _, rr := range m.Answer {
		if _, ok := rr.(*dns.OPT); ok {
			continue
		}
		t := rr.Header().Ttl
		if !set || t < ttl {
			ttl = t
			set = true
		}
	}
	if !set {
		for _, rr := range m.Ns {
			if soa, ok := rr.(*dns.SOA); ok {
				t := soa.Minttl
				if soa.Hdr.Ttl < t {
					t = soa.Hdr.Ttl
				}
				if !set || t < ttl {
					ttl = t
					set = true
				}
			}
		}
	}
	return ttl
}

// isNegative сообщает, является ли ответ негативным: NXDOMAIN либо NODATA
// (NOERROR без записей в Answer).
func isNegative(m *dns.Msg) bool {
	return m.Rcode == dns.RcodeNameError || (m.Rcode == dns.RcodeSuccess && len(m.Answer) == 0)
}
