package rules

import (
	"sort"
	"strings"
)

// Blocklist — набор заблокированных доменов. Совпадение засчитывается самому
// домену и всем его поддоменам (суффикс по границе метки): блокировка
// example.com блокирует a.b.example.com, но не notexample.com.
type Blocklist struct {
	zones []string // нормализованные зоны, длинные — раньше
}

// NewBlocklist компилирует список доменов. Записи нормализуются (нижний
// регистр, без "*." и завершающей точки), пустые отбрасываются.
func NewBlocklist(domains []string) *Blocklist {
	out := make([]string, 0, len(domains))
	seen := make(map[string]struct{}, len(domains))
	for _, d := range domains {
		z := normalizeZone(d)
		if z == "" {
			continue
		}
		if _, ok := seen[z]; ok {
			continue
		}
		seen[z] = struct{}{}
		out = append(out, z)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return len(out[i]) > len(out[j])
	})
	return &Blocklist{zones: out}
}

// Match возвращает совпавшую запись блоклиста (самую конкретную) и признак
// блокировки. Возвращённая запись позволяет UI показать, чем именно
// заблокирован домен: самим собой или родительским доменом.
func (b *Blocklist) Match(qname string) (string, bool) {
	if b == nil || len(b.zones) == 0 {
		return "", false
	}
	name := normalizeName(qname)
	for _, z := range b.zones {
		if name == z || strings.HasSuffix(name, "."+z) {
			return z, true
		}
	}
	return "", false
}

// Len возвращает число записей в блоклисте.
func (b *Blocklist) Len() int {
	if b == nil {
		return 0
	}
	return len(b.zones)
}

// Equal сообщает, содержат ли блоклисты одинаковый набор записей.
func (b *Blocklist) Equal(o *Blocklist) bool {
	if b.Len() != o.Len() {
		return false
	}
	if b.Len() == 0 {
		return true
	}
	// Оба набора нормализованы и отсортированы по длине, но при одинаковой
	// длине порядок может отличаться — сравниваем как множества.
	seen := make(map[string]struct{}, b.Len())
	for _, z := range b.zones {
		seen[z] = struct{}{}
	}
	for _, z := range o.zones {
		if _, ok := seen[z]; !ok {
			return false
		}
	}
	return true
}
