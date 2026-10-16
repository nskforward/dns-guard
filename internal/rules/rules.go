// Package rules реализует сопоставление доменных имён с правилами по зонам.
package rules

import (
	"sort"
	"strings"

	"github.com/nskforward/dns-guard/internal/config"
)

// rule — скомпилированное правило.
type rule struct {
	zone  string // нормализованная зона без завершающей точки, в нижнем регистре
	group string
}

// Matcher сопоставляет qname группе апстримов по самому длинному совпадению зоны.
type Matcher struct {
	rules []rule
}

// New компилирует правила. Правила сортируются по длине зоны (длинные — раньше),
// что даёт longest-suffix match за линейный проход.
func New(rs []config.Rule) *Matcher {
	out := make([]rule, 0, len(rs))
	for _, r := range rs {
		z := normalizeZone(r.Zone)
		if z == "" {
			continue
		}
		out = append(out, rule{zone: z, group: r.Group})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return len(out[i].zone) > len(out[j].zone)
	})
	return &Matcher{rules: out}
}

// Match возвращает имя группы для домена и признак совпадения.
func (m *Matcher) Match(qname string) (string, bool) {
	name := normalizeName(qname)
	for _, r := range m.rules {
		if name == r.zone || strings.HasSuffix(name, "."+r.zone) {
			return r.group, true
		}
	}
	return "", false
}

// normalizeZone приводит зону к каноническому виду: убирает "*." и завершающую точку.
func normalizeZone(z string) string {
	z = strings.ToLower(strings.TrimSpace(z))
	z = strings.TrimSuffix(z, ".")
	z = strings.TrimPrefix(z, "*.")
	z = strings.TrimPrefix(z, ".")
	return z
}

func normalizeName(n string) string {
	n = strings.ToLower(strings.TrimSpace(n))
	return strings.TrimSuffix(n, ".")
}
