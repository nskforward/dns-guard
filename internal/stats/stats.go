// Package stats собирает статистику обработанных DNS-запросов: общие счётчики,
// распределение по группам апстримов и по парам (домен, тип запроса). Для
// каждой пары хранится информация о последнем выданном ответе.
//
// Память на домен — O(1) по числу типов запросов, фоновых горутин нет. Число
// уникальных доменов ограничено maxDomains, число типов на домен —
// maxAnswersPerDomain.
package stats

import (
	"math"
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
	// latencySamples — сколько последних длительностей апстрима хранить для p90.
	latencySamples = 256
	// maxAnswersPerDomain — сколько типов запросов (и последних ответов по ним)
	// хранить на домен (A/AAAA/HTTPS/...); защита от роста памяти при переборе
	// qtype.
	maxAnswersPerDomain = 8
)

// DomainStat — строка таблицы статистики: одна пара (домен, тип запроса).
type DomainStat struct {
	Domain string `json:"domain"`
	// Type — тип запроса (A, AAAA, HTTPS, ...). Пусто, если запрос учтён без
	// сведений о типе.
	Type string `json:"type,omitempty"`
	// Status — состояние хронологически последнего ответа на запросы этого
	// типа: "ok" для успешного ответа, "servfail"/"nxdomain"/"nodata"/"blocked"/
	// "bad" для проблемного, пусто — если ответов ещё не было.
	Status    string `json:"status,omitempty"`
	Total     uint64 `json:"total"`
	CacheHits uint64 `json:"cache_hits"`
	// Blocked — домен (или его родитель) в блоклисте. BlockedBy — запись
	// блоклиста, которая сработала.
	Blocked   bool   `json:"blocked"`
	BlockedBy string `json:"blocked_by,omitempty"`
}

// AnswerInfo — сведения о последнем ответе, выданном по домену.
type AnswerInfo struct {
	Rcode      string        `json:"rcode"`
	Qtype      string        `json:"qtype,omitempty"`
	Records    []string      `json:"records,omitempty"`   // секция Answer
	Authority  []string      `json:"authority,omitempty"` // секция Ns (обычно SOA)
	Error      string        `json:"error,omitempty"`
	Upstream   string        `json:"upstream,omitempty"`
	FromCache  bool          `json:"from_cache"`
	DurationMs float64       `json:"duration_ms,omitempty"` // время запроса через апстрим, мс
	Attempts   []AttemptInfo `json:"attempts,omitempty"`    // цепочка попыток (только промах кэша)
	// Timeout — ответ сформирован, потому что апстрим не успел ответить за
	// отведённое время. Error при этом пуст: причина понятна из флага.
	Timeout bool `json:"timeout,omitempty"`
	// Blocked — ответ выдан блоклистом, а не апстримом; BlockedBy — сработавшая
	// запись блоклиста.
	Blocked   bool      `json:"blocked,omitempty"`
	BlockedBy string    `json:"blocked_by,omitempty"`
	At        time.Time `json:"at"`
}

// AttemptInfo — одна попытка обращения к апстриму группы: кто ответил, с каким
// результатом и за сколько. Цепочка показывает, к каким серверам ходили —
// например, NODATA от основного и последующая проверка резервными.
type AttemptInfo struct {
	Upstream   string  `json:"upstream"`
	Status     string  `json:"status,omitempty"` // OK / NODATA / NXDOMAIN / SERVFAIL / ERROR
	Error      string  `json:"error,omitempty"`  // текст ошибки транспортного уровня
	DurationMs float64 `json:"duration_ms,omitempty"`
}

// successful сообщает, что ответ отражает реально полученные записи: NOERROR
// и хотя бы одна запись в секции Answer. Негативные ответы (NXDOMAIN, NODATA)
// и SERVFAIL успешными не считаются.
func (a *AnswerInfo) successful() bool {
	return a.Rcode == "NOERROR" && len(a.Records) > 0
}

// DomainInfo — статистика домена и сводные сведения о том, что известно по
// домену: все записи, склеенные из последних успешных ответов по каждому типу
// запроса, и последний выданный ответ.
type DomainInfo struct {
	DomainStat
	Answer   *AnswerInfo  `json:"answer,omitempty"`   // склейка успешных ответов по qtype
	Negative []AnswerInfo `json:"negative,omitempty"` // последние NODATA/NXDOMAIN по qtype, свежие сверху
	// Last — последний выданный ответ любого типа (включая SERVFAIL). Именно
	// по нему строится Status в таблице доменов.
	Last *AnswerInfo `json:"last,omitempty"`
}

// UpstreamStat — счётчики попыток одного апстрима в группе.
type UpstreamStat struct {
	Upstream string  `json:"upstream"`
	Ok       uint64  `json:"ok"`
	Failed   uint64  `json:"failed"`
	P90Ms    float64 `json:"p90_ms,omitempty"` // 90-й перцентиль длительности, мс
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

// Snapshot — снимок общих счётчиков на момент вызова.
type Snapshot struct {
	Uptime      int64             `json:"uptime_seconds"`
	Total       uint64            `json:"total"`
	CacheHits   uint64            `json:"cache_hits"`
	CacheMisses uint64            `json:"cache_misses"`
	Servfail    uint64            `json:"servfail"`
	Blocked     uint64            `json:"blocked"`
	Bad         uint64            `json:"bad"`
	Nxdomain    uint64            `json:"nxdomain"`
	Nodata      uint64            `json:"nodata"`
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
	total    uint64                 // всего запросов по домену (все типы)
	hits     uint64                 // ответов из кэша по домену (все типы)
	types    map[string]*typeStat   // счётчики и последний ответ по каждому qtype
	answers  map[string]*AnswerInfo // последний успешный ответ по каждому qtype
	negative map[string]*AnswerInfo // последний негативный (NODATA/NXDOMAIN) по qtype
	last     *AnswerInfo            // последний выданный ответ любого типа
}

// typeStat — счётчики запросов одного типа к домену и последний ответ на них.
type typeStat struct {
	total uint64
	hits  uint64
	last  *AnswerInfo
}

// upstreamCounters — счётчики попыток одного апстрима.
type upstreamCounters struct {
	ok      uint64
	failed  uint64
	latency [latencySamples]float64 // кольцевой буфер длительностей, мс
	latN    int                     // заполнено слотов (<= latencySamples)
	latPos  int                     // следующий слот для записи
}

// addLatency сохраняет длительность попытки в кольцевом буфере.
func (c *upstreamCounters) addLatency(ms float64) {
	c.latency[c.latPos] = ms
	c.latPos++
	if c.latPos == latencySamples {
		c.latPos = 0
	}
	if c.latN < latencySamples {
		c.latN++
	}
}

// p90 возвращает 90-й перцентиль накопленных длительностей (метод nearest-rank).
func (c *upstreamCounters) p90() float64 {
	if c.latN == 0 {
		return 0
	}
	buf := make([]float64, c.latN)
	copy(buf, c.latency[:c.latN])
	sort.Float64s(buf)
	idx := int(math.Ceil(0.9*float64(len(buf)))) - 1
	if idx < 0 {
		idx = 0
	}
	return buf[idx]
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
	blocked     atomic.Uint64
	bad         atomic.Uint64
	nxdomain    atomic.Uint64
	nodata      atomic.Uint64

	mu        sync.Mutex
	domains   map[string]*domain
	groups    map[string]uint64
	groupHits map[string]uint64
	ups       map[string]map[string]*upstreamCounters
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
	blocked := info != nil && info.Blocked
	switch {
	case blocked:
		// Заблокированный запрос не ходил ни в кэш, ни к апстриму: он не
		// должен искажать статистику попаданий.
		s.blocked.Add(1)
	case hit:
		s.cacheHits.Add(1)
	default:
		s.cacheMisses.Add(1)
	}
	// Проблемные ответы считаются по всем выданным ответам, включая ответы
	// из кэша, чтобы карточка «Bad answers» в UI совпадала с фильтром bad=1.
	status := statusOf(info)
	switch status {
	case "servfail":
		s.servfail.Add(1)
	case "nxdomain":
		s.nxdomain.Add(1)
	case "nodata":
		s.nodata.Add(1)
	case "bad":
		s.bad.Add(1)
	}

	name := normalize(qname)
	at := s.now()

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
	d := s.domains[name]
	if d == nil {
		// Лимит достигнут: новые домены не учитываем, но общие счётчики
		// (total/hits/misses/servfail/groups) уже обновлены выше.
		if len(s.domains) >= s.maxDomains {
			return
		}
		d = &domain{}
		s.domains[name] = d
	}
	d.total++
	if hit {
		d.hits++
	}
	if info != nil {
		cp := cloneAnswer(info, at)
		// Ответ из кэша не несёт собственную цепочку попыток: она относится
		// к запросу, которым кэш был наполнен. Наследуем её, чтобы в карточке
		// последнего ответа цепочка была видна и при кэш-хите.
		if cp.FromCache && len(cp.Attempts) == 0 {
			cp.Attempts = d.inheritAttempts(cp.Qtype)
		}
		// Последний ответ любого типа — источник Status в карточке домена.
		d.last = &cp
		// Счётчики и последний ответ по типу запроса — источник строки таблицы.
		ts := d.typeStat(cp.Qtype)
		ts.total++
		if hit {
			ts.hits++
		}
		ts.last = &cp
		if cp.Blocked {
			// Заблокированный ответ сохраняется как ответ по типу: он
			// вытесняет прежние положительные записи, чтобы в сводке домена
			// не показывались адреса, которые больше не выдаются.
			d.storeAnswer(&cp)
		} else if cp.successful() {
			d.storeAnswer(&cp)
		} else if status != "servfail" {
			// NODATA (NOERROR без записей) и NXDOMAIN: успешные записи не
			// трогаем, но запоминаем последний негативный ответ по типу.
			d.storeNegative(&cp)
		}
	}
}

// inheritAttempts возвращает цепочку попыток, сохранённую для последнего
// ответа того же qtype (промаха кэша), чтобы кэш-хит показывал, какими
// серверами получен закэшированный ответ.
func (d *domain) inheritAttempts(qtype string) []AttemptInfo {
	for _, a := range []*AnswerInfo{d.answers[qtype], d.negative[qtype]} {
		if a != nil && len(a.Attempts) > 0 {
			return append([]AttemptInfo(nil), a.Attempts...)
		}
	}
	if d.last != nil && d.last.Qtype == qtype && len(d.last.Attempts) > 0 {
		return append([]AttemptInfo(nil), d.last.Attempts...)
	}
	return nil
}

// storeAnswer сохраняет последний успешный ответ по его qtype; негативные
// ответы (NXDOMAIN, NODATA) и SERVFAIL сюда не попадают и не затирают ранее
// накопленные записи. Число типов на домен ограничено maxAnswersPerDomain:
// при переполнении вытесняется самый старый. Свежий успешный ответ делает
// прежний негативный ответ по тому же типу неактуальным.
func (d *domain) storeAnswer(info *AnswerInfo) {
	if d.answers == nil {
		d.answers = make(map[string]*AnswerInfo)
	}
	if _, ok := d.answers[info.Qtype]; !ok {
		evictOldest(d.answers)
	}
	d.answers[info.Qtype] = info
	delete(d.negative, info.Qtype)
}

// storeNegative сохраняет последний негативный ответ (NODATA/NXDOMAIN) по его
// qtype. Успешную сводку не трогает: негативный ответ не затирает ранее
// полученные записи. Лимит типов тот же, что и для успешных ответов.
func (d *domain) storeNegative(info *AnswerInfo) {
	if d.negative == nil {
		d.negative = make(map[string]*AnswerInfo)
	}
	if _, ok := d.negative[info.Qtype]; !ok {
		evictOldest(d.negative)
	}
	d.negative[info.Qtype] = info
}

// typeStat возвращает счётчики типа запроса, создавая их при необходимости.
// Число типов на домен ограничено maxAnswersPerDomain: при переполнении
// вытесняется тип с самым старым последним ответом.
func (d *domain) typeStat(qtype string) *typeStat {
	if d.types == nil {
		d.types = make(map[string]*typeStat)
	}
	if ts := d.types[qtype]; ts != nil {
		return ts
	}
	if len(d.types) >= maxAnswersPerDomain {
		var (
			oldestKey string
			oldest    time.Time
			first     = true
		)
		for k, v := range d.types {
			var at time.Time
			if v.last != nil {
				at = v.last.At
			}
			if first || at.Before(oldest) {
				oldestKey, oldest, first = k, at, false
			}
		}
		delete(d.types, oldestKey)
	}
	ts := &typeStat{}
	d.types[qtype] = ts
	return ts
}

// evictOldest вытесняет самый старый по времени ответ, если лимит типов на
// домен исчерпан.
func evictOldest(m map[string]*AnswerInfo) {
	if len(m) < maxAnswersPerDomain {
		return
	}
	var (
		oldestKey string
		oldest    time.Time
		first     = true
	)
	for k, v := range m {
		if first || v.At.Before(oldest) {
			oldestKey, oldest, first = k, v.At, false
		}
	}
	delete(m, oldestKey)
}

// RecordUpstream учитывает одну попытку обращения к апстриму группы.
// failed — сервер не дал ответа (таймаут, сетевая ошибка и т.п.);
// elapsed — длительность попытки.
func (s *Stats) RecordUpstream(group, upstream string, failed bool, elapsed time.Duration) {
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
	c.addLatency(float64(elapsed) / float64(time.Millisecond))
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
				P90Ms:    c.p90(),
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

// DomainsPage возвращает страницу строк статистики: по одной строке на каждую
// пару (домен, тип запроса), отсортированных по числу запросов. search —
// подстрока имени без учёта регистра (пустая — все домены). page нумеруется с
// единицы; size по умолчанию DefaultPageSize и не больше MaxPageSize.
//
// isBlocked (может быть nil) проставляет в элементах Blocked/BlockedBy;
// blockedOnly оставляет только строки заблокированных доменов;
// badOnly оставляет только строки с проблемным последним ответом
// (bad/servfail/nxdomain/nodata), исключая ok, blocked и строки без ответов.
// status (пустая строка — без фильтра) оставляет только строки с указанным
// статусом последнего ответа: ok, servfail, nxdomain, nodata, blocked или bad.
func (s *Stats) DomainsPage(page, size int, search string, blockedOnly, badOnly bool, status string, isBlocked func(string) (string, bool)) DomainsPage {
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

	s.mu.Lock()
	out := make([]DomainStat, 0, len(s.domains))
	for name, d := range s.domains {
		if query != "" && !strings.Contains(name, query) {
			continue
		}
		var entry string
		var blocked bool
		if isBlocked != nil {
			entry, blocked = isBlocked(name)
		}
		if blockedOnly && !blocked {
			continue
		}
		// Строки по типам запросов. Если ответы учтены без сведений о типе
		// (info == nil), показываем одну строку без Type, чтобы домен не
		// пропадал из таблицы.
		if len(d.types) == 0 {
			st := DomainStat{
				Domain:    name,
				Status:    statusOf(d.last),
				Total:     d.total,
				CacheHits: d.hits,
				Blocked:   blocked,
				BlockedBy: entry,
			}
			if blocked {
				st.Status = "blocked"
			}
			if badOnly && !isBadStatus(st.Status) {
				continue
			}
			if status != "" && st.Status != status {
				continue
			}
			out = append(out, st)
			continue
		}
		for qtype, ts := range d.types {
			st := DomainStat{
				Domain:    name,
				Type:      qtype,
				Status:    statusOf(ts.last),
				Total:     ts.total,
				CacheHits: ts.hits,
				Blocked:   blocked,
				BlockedBy: entry,
			}
			if blocked {
				// Домен заблокирован прямо сейчас: статус важнее последнего
				// сохранённого ответа по типу.
				st.Status = "blocked"
			}
			if badOnly && !isBadStatus(st.Status) {
				continue
			}
			if status != "" && st.Status != status {
				continue
			}
			out = append(out, st)
		}
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].Type < out[j].Type
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

// DomainInfo возвращает статистику домена и сводные сведения о нём: записи,
// склеенные из последних успешных ответов по всем типам запроса.
func (s *Stats) DomainInfo(name string) (DomainInfo, bool) {
	key := normalize(name)
	if key == "" {
		return DomainInfo{}, false
	}

	s.mu.Lock()
	d, ok := s.domains[key]
	if !ok {
		s.mu.Unlock()
		return DomainInfo{}, false
	}
	info := DomainInfo{DomainStat: domainStat(key, d)}
	if d.last != nil {
		cp := cloneAnswer(d.last, d.last.At)
		info.Last = &cp
	}
	if len(d.answers) > 0 {
		a := mergedAnswer(d.answers)
		info.Answer = &a
	}
	if len(d.negative) > 0 {
		info.Negative = mergedNegative(d.negative)
	}
	s.mu.Unlock()

	return info, true
}

// mergedNegative возвращает последние негативные ответы по каждому qtype,
// свежие сверху. Записи не склеиваются: у NODATA/NXDOMAIN их нет, а тип
// запроса важен для понимания, что именно проверялось.
func mergedNegative(negative map[string]*AnswerInfo) []AnswerInfo {
	out := make([]AnswerInfo, 0, len(negative))
	for _, a := range negative {
		out = append(out, cloneAnswer(a, a.At))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Qtype < out[j].Qtype
	})
	return out
}

// mergedAnswer склеивает последние успешные ответы по всем qtype в один:
// записи объединяются и дедуплицируются, а атрибуты (источник, апстрим,
// длительность, время) берутся у самого свежего ответа.
func mergedAnswer(answers map[string]*AnswerInfo) AnswerInfo {
	var latest *AnswerInfo
	for _, a := range answers {
		if latest == nil || a.At.After(latest.At) ||
			(a.At.Equal(latest.At) && a.Qtype < latest.Qtype) {
			latest = a
		}
	}
	out := cloneAnswer(latest, latest.At)
	// Сводный ответ не относится к одному типу запроса.
	out.Qtype = ""
	out.Records = mergeUnique(answers, func(a *AnswerInfo) []string { return a.Records })
	out.Authority = mergeUnique(answers, func(a *AnswerInfo) []string { return a.Authority })
	return out
}

// mergeUnique объединяет выбранные строки всех ответов, убирает дубликаты и
// сортирует результат, чтобы порядок записей был стабильным.
func mergeUnique(answers map[string]*AnswerInfo, pick func(*AnswerInfo) []string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, a := range answers {
		for _, line := range pick(a) {
			if _, ok := seen[line]; ok {
				continue
			}
			seen[line] = struct{}{}
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

// cloneAnswer делает глубокую копию AnswerInfo, подставляя at, если время не
// задано.
func cloneAnswer(info *AnswerInfo, at time.Time) AnswerInfo {
	cp := *info
	cp.Records = append([]string(nil), info.Records...)
	cp.Authority = append([]string(nil), info.Authority...)
	cp.Attempts = append([]AttemptInfo(nil), info.Attempts...)
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
		Blocked:     s.blocked.Load(),
		Bad:         s.bad.Load(),
		Nxdomain:    s.nxdomain.Load(),
		Nodata:      s.nodata.Load(),
		Domains:     domains,
		Groups:      groups,
	}
}

// domainStat собирает сводную строку домена (без типа) для карточки деталей.
func domainStat(name string, d *domain) DomainStat {
	return DomainStat{
		Domain:    name,
		Status:    statusOf(d.last),
		Total:     d.total,
		CacheHits: d.hits,
	}
}

// isBadStatus сообщает, что статус последнего ответа считается проблемным:
// ошибка, SERVFAIL, NXDOMAIN или NODATA. ok, blocked и пустой статус
// проблемными не считаются.
func isBadStatus(status string) bool {
	switch status {
	case "bad", "servfail", "nxdomain", "nodata":
		return true
	}
	return false
}

// statusOf классифицирует последний ответ домена для колонки Status.
func statusOf(a *AnswerInfo) string {
	switch {
	case a == nil:
		return ""
	case a.Blocked:
		return "blocked"
	case a.Rcode == "SERVFAIL":
		return "servfail"
	case a.Rcode == "NXDOMAIN":
		return "nxdomain"
	case a.successful():
		return "ok"
	case a.Rcode == "NOERROR":
		return "nodata"
	default:
		return "bad"
	}
}

// normalize приводит имя к каноническому виду: нижний регистр без завершающей
// точки. Полное имя сохраняется без сокращения до домена второго уровня.
func normalize(qname string) string {
	name := strings.ToLower(strings.TrimSpace(qname))
	return strings.TrimSuffix(name, ".")
}
