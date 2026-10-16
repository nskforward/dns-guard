// Package stats собирает статистику обработанных DNS-запросов: общие счётчики,
// распределение по группам апстримов и по доменам второго уровня с пиковым RPM.
//
// Память на домен — O(1), фоновых горутин нет: смена минуты обрабатывается
// лениво при записи/чтении.
package stats

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DomainStat — статистика по домену второго уровня.
type DomainStat struct {
	Domain  string `json:"domain"`
	Total   uint64 `json:"total"`
	Current uint64 `json:"current_minute"`
	PeakRPM uint64 `json:"peak_rpm"`
}

// Snapshot — снимок статистики на момент вызова.
type Snapshot struct {
	Uptime      int64             `json:"uptime_seconds"`
	Total       uint64            `json:"total"`
	CacheHits   uint64            `json:"cache_hits"`
	CacheMisses uint64            `json:"cache_misses"`
	Servfail    uint64            `json:"servfail"`
	Domains     int               `json:"domains"`
	Groups      map[string]uint64 `json:"groups"`
	Top         []DomainStat      `json:"top"`
}

// domain — счётчики одного домена второго уровня.
type domain struct {
	minute int64  // unix/60 минуты последней записи
	count  uint64 // запросов в текущей минуте
	peak   uint64 // максимум count за завершённые минуты
	total  uint64 // всего запросов
}

// Stats — потокобезопасный сборщик статистики.
type Stats struct {
	start time.Time
	now   func() time.Time

	total       atomic.Uint64
	cacheHits   atomic.Uint64
	cacheMisses atomic.Uint64
	servfail    atomic.Uint64

	mu      sync.Mutex
	domains map[string]*domain
	groups  map[string]uint64
}

// New создаёт сборщик статистики.
func New() *Stats { return newWithClock(time.Now) }

func newWithClock(now func() time.Time) *Stats {
	return &Stats{
		start:   now(),
		now:     now,
		domains: make(map[string]*domain),
		groups:  make(map[string]uint64),
	}
}

// Record учитывает один обработанный запрос.
// qname — имя из вопроса, group — обслужившая группа, hit — ответ из кэша,
// isServfail — итоговый код ответа SERVFAIL.
func (s *Stats) Record(qname, group string, hit, isServfail bool) {
	s.total.Add(1)
	if hit {
		s.cacheHits.Add(1)
	} else {
		s.cacheMisses.Add(1)
	}
	if isServfail {
		s.servfail.Add(1)
	}

	name := baseDomain(qname)
	mid := s.now().Unix() / 60

	s.mu.Lock()
	defer s.mu.Unlock()
	if group != "" {
		s.groups[group]++
	}
	if name == "" {
		return
	}
	d := s.domains[name]
	if d == nil {
		d = &domain{minute: mid}
		s.domains[name] = d
	}
	if d.minute != mid {
		if d.count > d.peak {
			d.peak = d.count
		}
		d.count = 0
		d.minute = mid
	}
	d.count++
	d.total++
}

// TopN возвращает до n доменов, отсортированных по общему числу запросов.
// Текущая минута каждого домена финализируется на момент вызова.
func (s *Stats) TopN(n int) []DomainStat {
	mid := s.now().Unix() / 60

	s.mu.Lock()
	out := make([]DomainStat, 0, len(s.domains))
	for name, d := range s.domains {
		peak := d.peak
		current := d.count
		if d.minute != mid {
			if current > peak {
				peak = current
			}
			current = 0
		}
		out = append(out, DomainStat{
			Domain:  name,
			Total:   d.total,
			Current: current,
			PeakRPM: peak,
		})
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Domain < out[j].Domain
	})
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Snapshot возвращает общие счётчики, распределение по группам и top-N доменов.
func (s *Stats) Snapshot(topN int) Snapshot {
	s.mu.Lock()
	groups := make(map[string]uint64, len(s.groups))
	for k, v := range s.groups {
		groups[k] = v
	}
	domains := len(s.domains)
	s.mu.Unlock()

	return Snapshot{
		Uptime:      int64(s.now().Sub(s.start).Seconds()),
		Total:       s.total.Load(),
		CacheHits:   s.cacheHits.Load(),
		CacheMisses: s.cacheMisses.Load(),
		Servfail:    s.servfail.Load(),
		Domains:     domains,
		Groups:      groups,
		Top:         s.TopN(topN),
	}
}

// baseDomain сводит имя к домену второго уровня (две последние метки).
func baseDomain(qname string) string {
	name := strings.ToLower(strings.TrimSpace(qname))
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return ""
	}
	labels := strings.Split(name, ".")
	if len(labels) <= 2 {
		return name
	}
	return labels[len(labels)-2] + "." + labels[len(labels)-1]
}
