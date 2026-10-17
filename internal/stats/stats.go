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
	// servfailEventsCap — сколько последних SERVFAIL-событий хранить в журнале.
	servfailEventsCap = 100
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

// UpstreamStat — счётчики попыток одного апстрима в группе.
type UpstreamStat struct {
	Upstream string `json:"upstream"`
	Ok       uint64 `json:"ok"`
	Failed   uint64 `json:"failed"`
}

// GroupStat — статистика по группе апстримов: запросы, кэш-хиты и разбивка
// по серверам группы. Total учитывает все запросы, обслуженные группой;
// Upstreams — только реальные обращения к серверам (промахи кэша).
type GroupStat struct {
	Name      string         `json:"name"`
	Total     uint64         `json:"total"`
	CacheHits uint64         `json:"cache_hits"`
	Failed    uint64         `json:"failed"`
	Upstreams []UpstreamStat `json:"upstreams"`
}

// ServfailEvent — одно событие SERVFAIL: запрос, на который не удалось получить
// ответ. Error заполнен при транспортной ошибке апстримов, Upstream — при
// SERVFAIL, полученном от самого сервера или из кэша.
type ServfailEvent struct {
	At        time.Time `json:"at"`
	Domain    string    `json:"domain"`
	Qtype     string    `json:"qtype"`
	Group     string    `json:"group,omitempty"`
	Upstream  string    `json:"upstream,omitempty"`
	Error     string    `json:"error,omitempty"`
	FromCache bool      `json:"from_cache"`
}

// ServfailDomain — домен с числом SERVFAIL и сведениями о последней ошибке.
type ServfailDomain struct {
	Domain string      `json:"domain"`
	Count  uint64      `json:"count"`
	Last   *AnswerInfo `json:"last,omitempty"`
}

// ServfailPage — домены с SERVFAIL (после сортировки и пагинации) и журнал
// последних событий. Total — общий счётчик SERVFAIL, DomainsTotal — число
// доменов, у которых есть хотя бы один SERVFAIL.
type ServfailPage struct {
	Total        uint64           `json:"total"`
	DomainsTotal int              `json:"domains_total"`
	Page         int              `json:"page"`
	Pages        int              `json:"pages"`
	Size         int              `json:"size"`
	Domains      []ServfailDomain `json:"domains"`
	Recent       []ServfailEvent  `json:"recent"`
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
	minute   int64       // unix/60 минуты последней записи
	count    uint64      // запросов в текущей минуте
	peak     uint64      // максимум count за завершённые минуты
	total    uint64      // всего запросов
	servfail uint64      // SERVFAIL-ответов
	last     *AnswerInfo // последний выданный ответ
	lastFail *AnswerInfo // последний SERVFAIL-ответ
}

// upstreamCounters — счётчики попыток одного апстрима.
type upstreamCounters struct {
	ok     uint64
	failed uint64
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

	mu             sync.Mutex
	domains        map[string]*domain
	groups         map[string]uint64
	groupHits      map[string]uint64
	ups            map[string]map[string]*upstreamCounters
	servfailEvents []ServfailEvent // кольцевой буфер, не больше servfailEventsCap
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
		groupHits:  make(map[string]uint64),
		ups:        make(map[string]map[string]*upstreamCounters),
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
	failed := info != nil && info.Rcode == "SERVFAIL"
	if failed {
		s.servfail.Add(1)
	}

	name := normalize(qname)
	at := s.now()
	mid := at.Unix() / 60

	s.mu.Lock()
	defer s.mu.Unlock()
	if group != "" {
		s.groups[group]++
		if hit {
			s.groupHits[group]++
		}
	}
	if name == "" {
		return
	}
	if failed {
		// Журнал событий не зависит от лимита доменов: он ведётся и тогда,
		// когда новые домены уже не попадают в таблицу.
		if len(s.servfailEvents) >= servfailEventsCap {
			s.servfailEvents = s.servfailEvents[1:]
		}
		s.servfailEvents = append(s.servfailEvents, ServfailEvent{
			At:        at,
			Domain:    name,
			Qtype:     info.Qtype,
			Group:     group,
			Upstream:  info.Upstream,
			Error:     info.Error,
			FromCache: info.FromCache,
		})
	}
	d := s.domains[name]
	if d == nil {
		// Лимит достигнут: новые домены не учитываем, но общие счётчики
		// (total/hits/misses/servfail/groups/events) уже обновлены выше.
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
		cp := cloneAnswer(info, at)
		d.last = &cp
		if failed {
			d.servfail++
			d.lastFail = &cp
		}
	}
}

// RecordUpstream учитывает одну попытку обращения к апстриму группы.
// failed — сервер не дал ответа (таймаут, сетевая ошибка и т.п.).
func (s *Stats) RecordUpstream(group, upstream string, failed bool) {
	if group == "" || upstream == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.ups[group]
	if m == nil {
		m = make(map[string]*upstreamCounters)
		s.ups[group] = m
	}
	c := m[upstream]
	if c == nil {
		c = &upstreamCounters{}
		m[upstream] = c
	}
	if failed {
		c.failed++
	} else {
		c.ok++
	}
}

// UpstreamsPage возвращает статистику по группам апстримов: группы по убыванию
// общего числа запросов, внутри группы — серверы по убыванию числа попыток.
func (s *Stats) UpstreamsPage() []GroupStat {
	s.mu.Lock()
	out := make([]GroupStat, 0, len(s.groups)+len(s.ups))
	seen := make(map[string]struct{}, len(s.groups)+len(s.ups))
	add := func(name string) {
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		gs := GroupStat{Name: name, Total: s.groups[name], CacheHits: s.groupHits[name]}
		for upstream, c := range s.ups[name] {
			gs.Upstreams = append(gs.Upstreams, UpstreamStat{
				Upstream: upstream,
				Ok:       c.ok,
				Failed:   c.failed,
			})
			gs.Failed += c.failed
		}
		out = append(out, gs)
	}
	for name := range s.groups {
		add(name)
	}
	for name := range s.ups {
		add(name)
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Name < out[j].Name
	})
	for i := range out {
		sort.Slice(out[i].Upstreams, func(a, b int) bool {
			x, y := out[i].Upstreams[a], out[i].Upstreams[b]
			if tx, ty := x.Ok+x.Failed, y.Ok+y.Failed; tx != ty {
				return tx > ty
			}
			return x.Upstream < y.Upstream
		})
	}
	return out
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
		cp := cloneAnswer(d.last, d.last.At)
		info.Last = &cp
	}
	s.mu.Unlock()
	return info, true
}

// ServfailPage возвращает домены с SERVFAIL, отсортированные по убыванию
// счётчика, и журнал последних событий (новые сверху). page нумеруется с
// единицы; size по умолчанию DefaultPageSize и не больше MaxPageSize.
func (s *Stats) ServfailPage(page, size int) ServfailPage {
	if size <= 0 {
		size = DefaultPageSize
	}
	if size > MaxPageSize {
		size = MaxPageSize
	}
	if page < 1 {
		page = 1
	}

	s.mu.Lock()
	items := make([]ServfailDomain, 0, len(s.domains))
	for name, d := range s.domains {
		if d.servfail == 0 {
			continue
		}
		sd := ServfailDomain{Domain: name, Count: d.servfail}
		if d.lastFail != nil {
			cp := cloneAnswer(d.lastFail, d.lastFail.At)
			sd.Last = &cp
		}
		items = append(items, sd)
	}
	recent := make([]ServfailEvent, 0, len(s.servfailEvents))
	for i := len(s.servfailEvents) - 1; i >= 0; i-- {
		recent = append(recent, s.servfailEvents[i])
	}
	s.mu.Unlock()

	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Domain < items[j].Domain
	})

	total := len(items)
	pages := 0
	if total > 0 {
		pages = (total + size - 1) / size
		if page > pages {
			page = pages
		}
	}
	out := ServfailPage{
		Total:        s.servfail.Load(),
		DomainsTotal: total,
		Page:         page,
		Pages:        pages,
		Size:         size,
		Domains:      []ServfailDomain{},
		Recent:       recent,
	}
	start := (page - 1) * size
	if start >= total {
		return out
	}
	end := start + size
	if end > total {
		end = total
	}
	out.Domains = items[start:end]
	return out
}

// cloneAnswer делает глубокую копию AnswerInfo, подставляя at, если время не
// задано.
func cloneAnswer(info *AnswerInfo, at time.Time) AnswerInfo {
	cp := *info
	cp.Records = append([]string(nil), info.Records...)
	cp.Authority = append([]string(nil), info.Authority...)
	if cp.At.IsZero() {
		cp.At = at
	}
	return cp
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
