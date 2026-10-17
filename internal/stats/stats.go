// Package stats собирает статистику обработанных DNS-запросов: общие счётчики,
// распределение по группам апстримов и по полным доменным именам с пиковым RPM.
// Для каждого домена хранится информация о последнем выданном ответе.
//
// Память на домен — O(1), фоновых горутин нет: смена минуты обрабатывается
// лениво при записи/чтении. Число уникальных доменов ограничено maxDomains.
package stats

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultMaxDomains — лимит уникальных доменов по умолчанию.
	DefaultMaxDomains = 50000
	// DefaultPageSize — размер страницы списка доменов по умолчанию.
	DefaultPageSize = 50
	// MaxPageSize — максимально допустимый размер страницы.
	MaxPageSize = 500
)

// DomainStat — статистика по полному доменному имени.
type DomainStat struct {
	Domain  string `json:"domain"`
	Total   uint64 `json:"total"`
	Current uint64 `json:"current_minute"`
	PeakRPM uint64 `json:"peak_rpm"`
}

// AnswerInfo — сведения о последнем ответе, выданном по домену.
type AnswerInfo struct {
	Rcode     string    `json:"rcode"`
	Qtype     string    `json:"qtype"`
	Records   []string  `json:"records,omitempty"`   // секция Answer
	Authority []string  `json:"authority,omitempty"` // секция Ns (обычно SOA)
	Error     string    `json:"error,omitempty"`
	Upstream  string    `json:"upstream,omitempty"`
	FromCache bool      `json:"from_cache"`
	At        time.Time `json:"at"`
}

// DomainInfo — статистика домена вместе с последним ответом.
type DomainInfo struct {
	DomainStat
	Last *AnswerInfo `json:"last,omitempty"`
}

// Snapshot — снимок общих счётчиков на момент вызова.
type Snapshot struct {
	Uptime      int64             `json:"uptime_seconds"`
	Total       uint64            `json:"total"`
	CacheHits   uint64            `json:"cache_hits"`
	CacheMisses uint64            `json:"cache_misses"`
	Servfail    uint64            `json:"servfail"`
	Domains     int               `json:"domains"`
	Groups      map[string]uint64 `json:"groups"`
}

// DomainsPage — страница списка доменов (после поиска и сортировки).
type DomainsPage struct {
	Total int          `json:"total"`
	Page  int          `json:"page"`
	Pages int          `json:"pages"`
	Size  int          `json:"size"`
	Items []DomainStat `json:"items"`
}

// domain — счётчики одного домена.
type domain struct {
	minute int64       // unix/60 минуты последней записи
	count  uint64      // запросов в текущей минуте
	peak   uint64      // максимум count за завершённые минуты
	total  uint64      // всего запросов
	last   *AnswerInfo // последний выданный ответ
}

// Stats — потокобезопасный сборщик статистики.
type Stats struct {
	start time.Time
	now   func() time.Time

	maxDomains int // лимит уникальных доменов (0 = DefaultMaxDomains)

	total       atomic.Uint64
	cacheHits   atomic.Uint64
	cacheMisses atomic.Uint64
	servfail    atomic.Uint64

	mu      sync.Mutex
	domains map[string]*domain
	groups  map[string]uint64
}

// New создаёт сборщик статистики. maxDomains <= 0 означает DefaultMaxDomains.
func New(maxDomains int) *Stats { return newWithClock(maxDomains, time.Now) }

func newWithClock(maxDomains int, now func() time.Time) *Stats {
	if maxDomains <= 0 {
		maxDomains = DefaultMaxDomains
	}
	return &Stats{
		start:      now(),
		now:        now,
		maxDomains: maxDomains,
		domains:    make(map[string]*domain),
		groups:     make(map[string]uint64),
	}
}

// SetMaxDomains меняет лимит уникальных доменов. Уже накопленные домены не
// удаляются: лимит ограничивает только добавление новых.
func (s *Stats) SetMaxDomains(n int) {
	if n <= 0 {
		n = DefaultMaxDomains
	}
	s.mu.Lock()
	s.maxDomains = n
	s.mu.Unlock()
}

// Record учитывает один обработанный запрос.
// qname — имя из вопроса, group — обслужившая группа, hit — ответ из кэша,
// info — сведения о выданном ответе (может быть nil).
func (s *Stats) Record(qname, group string, hit bool, info *AnswerInfo) {
	s.total.Add(1)
	if hit {
		s.cacheHits.Add(1)
	} else {
		s.cacheMisses.Add(1)
	}
	if info != nil && info.Rcode == "SERVFAIL" {
		s.servfail.Add(1)
	}

	name := normalize(qname)
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
		// Лимит достигнут: новые домены не учитываем, но общие счётчики
		// (total/hits/misses/servfail/groups) уже обновлены выше.
		if len(s.domains) >= s.maxDomains {
			return
		}
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
	if info != nil {
		cp := *info
		cp.Records = append([]string(nil), info.Records...)
		cp.Authority = append([]string(nil), info.Authority...)
		if cp.At.IsZero() {
			cp.At = s.now()
		}
		d.last = &cp
	}
}

// DomainsPage возвращает страницу доменов, отсортированных по общему числу
// запросов. search — подстрока имени без учёта регистра (пустая — все домены).
// page нумеруется с единицы; size по умолчанию DefaultPageSize и не больше
// MaxPageSize. Текущая минута каждого домена финализируется на момент вызова.
func (s *Stats) DomainsPage(page, size int, search string) DomainsPage {
	if size <= 0 {
		size = DefaultPageSize
	}
	if size > MaxPageSize {
		size = MaxPageSize
	}
	if page < 1 {
		page = 1
	}
	query := strings.ToLower(strings.TrimSpace(search))
	mid := s.now().Unix() / 60

	s.mu.Lock()
	out := make([]DomainStat, 0, len(s.domains))
	for name, d := range s.domains {
		if query != "" && !strings.Contains(name, query) {
			continue
		}
		out = append(out, statOf(name, d, mid))
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Domain < out[j].Domain
	})

	total := len(out)
	pages := 0
	if total > 0 {
		pages = (total + size - 1) / size
		if page > pages {
			page = pages
		}
	}
	start := (page - 1) * size
	if start >= total {
		return DomainsPage{Total: total, Page: page, Pages: pages, Size: size, Items: []DomainStat{}}
	}
	end := start + size
	if end > total {
		end = total
	}
	return DomainsPage{Total: total, Page: page, Pages: pages, Size: size, Items: out[start:end]}
}

// DomainInfo возвращает статистику домена и сведения о последнем ответе.
func (s *Stats) DomainInfo(name string) (DomainInfo, bool) {
	key := normalize(name)
	if key == "" {
		return DomainInfo{}, false
	}
	mid := s.now().Unix() / 60

	s.mu.Lock()
	d, ok := s.domains[key]
	if !ok {
		s.mu.Unlock()
		return DomainInfo{}, false
	}
	info := DomainInfo{DomainStat: statOf(key, d, mid)}
	if d.last != nil {
		cp := *d.last
		cp.Records = append([]string(nil), d.last.Records...)
		cp.Authority = append([]string(nil), d.last.Authority...)
		info.Last = &cp
	}
	s.mu.Unlock()
	return info, true
}

// Snapshot возвращает общие счётчики и распределение по группам.
func (s *Stats) Snapshot() Snapshot {
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
	}
}

// statOf собирает DomainStat, финализируя текущую минуту на момент mid.
func statOf(name string, d *domain, mid int64) DomainStat {
	peak := d.peak
	current := d.count
	if d.minute != mid {
		if current > peak {
			peak = current
		}
		current = 0
	}
	return DomainStat{
		Domain:  name,
		Total:   d.total,
		Current: current,
		PeakRPM: peak,
	}
}

// normalize приводит имя к каноническому виду: нижний регистр без завершающей
// точки. Полное имя сохраняется без сокращения до домена второго уровня.
func normalize(qname string) string {
	name := strings.ToLower(strings.TrimSpace(qname))
	return strings.TrimSuffix(name, ".")
}
