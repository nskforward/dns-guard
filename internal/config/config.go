// Package config описывает конфигурацию dns-guard: слушатели, кэш,
// группы апстримов и правила маршрутизации по зонам.
package config

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config — корневая конфигурация сервиса.
type Config struct {
	Listen       Listen           `yaml:"listen"`
	Web          Web              `yaml:"web"`
	Cache        Cache            `yaml:"cache"`
	Stats        Stats            `yaml:"stats"`
	Upstream     Upstream         `yaml:"upstream"`
	ECS          ECS              `yaml:"ecs,omitempty"`
	Block        Block            `yaml:"block,omitempty"`
	Groups       map[string]Group `yaml:"groups"`
	DefaultGroup string           `yaml:"default_group"`
	Rules        []Rule           `yaml:"rules"`
}

// Listen — адреса, на которых сервис принимает DNS-запросы.
type Listen struct {
	UDP string `yaml:"udp"`
	TCP string `yaml:"tcp"`
}

// Web — параметры встроенного веб-интерфейса.
type Web struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

// Cache — параметры in-memory кэша DNS-ответов.
type Cache struct {
	Enabled    bool   `yaml:"enabled"`
	MaxEntries int    `yaml:"max_entries"`
	MinTTL     uint32 `yaml:"min_ttl"` // минимальный TTL в секундах (пол)
	MaxTTL     uint32 `yaml:"max_ttl"` // максимальный TTL в секундах (0 = без ограничения)
	// NegTTL — потолок TTL негативных ответов (NXDOMAIN / NODATA по SOA minttl),
	// в секундах. 0 — не ограничивать сверх max_ttl и SOA. Защищает от
	// длительной «консервации» ошибочного негативного ответа апстрима.
	NegTTL uint32 `yaml:"neg_ttl"`
}

// Stats — параметры сбора статистики.
type Stats struct {
	// MaxDomains — лимит уникальных доменов в таблице статистики.
	MaxDomains int `yaml:"max_domains"`
}

// Upstream — общие параметры обращения к вышестоящим серверам.
type Upstream struct {
	Timeout string `yaml:"timeout"` // duration, по умолчанию 3s
}

// ECS — параметры EDNS Client Subnet (RFC 7871): в запрос к вышестоящему
// серверу добавляется наша публичная подсеть, чтобы гео-зависимые CDN
// возвращали адреса, близкие к нам, а не к локации апстрима.
type ECS struct {
	Enabled bool `yaml:"enabled"`
	// Subnet — публичный IPv4-префикс, например "203.0.113.0/24". Более
	// длинный префикс нормализуется до /24: передача точного адреса избыточна
	// для геолокации и снижает приватность.
	Subnet string `yaml:"subnet,omitempty"`
	// IPv6Subnet — публичный IPv6-префикс; нормализуется до /56.
	IPv6Subnet string `yaml:"ipv6_subnet,omitempty"`
}

// Режимы ответа для заблокированных доменов.
const (
	// BlockModeNoData — пустой ответ NOERROR без записей.
	BlockModeNoData = "nodata"
	// BlockModeZero — фиктивный адрес 0.0.0.0 (A) или :: (AAAA).
	BlockModeZero = "zero"
)

// Block — блокировка доменов и всех их поддоменов: запросы не уходят к
// апстриму, а получают ответ, по которому невозможно установить соединение.
type Block struct {
	// Mode — nodata (по умолчанию) или zero. Пустое значение эквивалентно
	// nodata, но не сериализуется в YAML, чтобы не засорять конфиг.
	Mode string `yaml:"mode,omitempty"`
	// Domains — заблокированные домены; поддомены блокируются автоматически.
	Domains []string `yaml:"domains,omitempty"`
}

// ModeOrDefault возвращает режим блокировки, подставляя режим по умолчанию.
func (b Block) ModeOrDefault() string {
	if b.Mode == BlockModeZero {
		return BlockModeZero
	}
	return BlockModeNoData
}

// NormalizeDomain приводит доменное имя к каноническому виду: нижний регистр,
// без "*." и завершающей точки.
func NormalizeDomain(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimSuffix(name, ".")
	name = strings.TrimPrefix(name, "*.")
	name = strings.TrimPrefix(name, ".")
	return name
}

// validDomainName проверяет, что нормализованное имя пригодно для DNS-запроса:
// непустое, разумной длины и без пробелов/служебных символов.
func validDomainName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == '/' || r == '\\' || r == 0x7f {
			return false
		}
	}
	return true
}

// ParseECSNet разбирает CIDR-подсеть и обрезает префикс до рекомендуемого
// максимума (family=4 → /24, family=6 → /56), возвращая нормализованную сеть.
func ParseECSNet(s string, family int) (*net.IPNet, error) {
	ip, n, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	ones, bits := n.Mask.Size()
	var maxBits int
	switch family {
	case 4:
		maxBits = 24
		if bits != 32 || ip.To4() == nil {
			return nil, fmt.Errorf("not an IPv4 subnet: %q", s)
		}
		ip = ip.To4()
	case 6:
		maxBits = 56
		if bits != 128 || ip.To4() != nil {
			return nil, fmt.Errorf("not an IPv6 subnet: %q", s)
		}
	default:
		return nil, fmt.Errorf("unknown IP family %d", family)
	}
	if ones > maxBits {
		ones = maxBits
	}
	mask := net.CIDRMask(ones, bits)
	return &net.IPNet{IP: ip.Mask(mask), Mask: mask}, nil
}

// ECSNets возвращает нормализованные статические подсети для ECS (nil, если
// соответствующая подсеть не задана). Конфигурация предполагается валидной.
func (c *Config) ECSNets() (v4, v6 *net.IPNet) {
	if s := strings.TrimSpace(c.ECS.Subnet); s != "" {
		if n, err := ParseECSNet(s, 4); err == nil {
			v4 = n
		}
	}
	if s := strings.TrimSpace(c.ECS.IPv6Subnet); s != "" {
		if n, err := ParseECSNet(s, 6); err == nil {
			v6 = n
		}
	}
	return v4, v6
}

// Group — группа апстримов с единым протоколом и набором primary/fallback.
type Group struct {
	Protocol  string   `yaml:"protocol"`  // udp | dot | doh
	Primary   []string `yaml:"primary"`   // основные серверы
	Fallback  []string `yaml:"fallback"`  // резервные серверы
	Bootstrap []string `yaml:"bootstrap"` // plain-DNS для резолва DoH/DoT хостнеймов
	// Hosts — статический маппинг хостнейм→IP для DoH/DoT: используется, когда
	// резолв хостнейма апстрима недоступен (bootstrap-UDP закрыт). Хостнейм в
	// URL сохраняется для TLS-SNI/проверки сертификата, а соединение идёт на IP.
	Hosts map[string]string `yaml:"hosts"`
	// ECS перекрывает глобальную настройку ECS для группы: false отключает
	// передачу клиентской подсети (нужно для зон, которые специально должны
	// резолвиться с локацией удалённого апстрима, например для VPN).
	ECS *bool `yaml:"ecs,omitempty"`
}

// Rule — правило маршрутизации зоны на группу апстримов.
type Rule struct {
	Zone  string `yaml:"zone"`  // например "ru" или "example.com"
	Group string `yaml:"group"` // имя группы
}

// Timeout возвращает таймаут обращения к апстриму.
func (c *Config) Timeout() time.Duration {
	if c.Upstream.Timeout == "" {
		return 3 * time.Second
	}
	d, err := time.ParseDuration(c.Upstream.Timeout)
	if err != nil || d <= 0 {
		return 3 * time.Second
	}
	return d
}

// Default возвращает конфигурацию по умолчанию.
func Default() *Config {
	return &Config{
		Listen: Listen{UDP: ":53", TCP: ":53"},
		Web:    Web{Enabled: true, Listen: ":80"},
		Cache:  Cache{Enabled: true, MaxEntries: 10000, MinTTL: 60, MaxTTL: 0},
		Stats:  Stats{MaxDomains: 50000},
		Upstream: Upstream{
			Timeout: "3s",
		},
		Groups: map[string]Group{
			"default": {
				Protocol: "udp",
				Primary:  []string{"77.88.8.8", "8.8.8.8"},
				Fallback: []string{"1.1.1.1"},
			},
		},
		DefaultGroup: "default",
		Rules:        []Rule{},
	}
}

// Load читает конфигурацию из файла. Если файла нет — возвращает Default.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return nil, err
	}
	return Parse(data)
}

// Parse разбирает и валидирует конфигурацию из байт.
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	// Карта групп должна задаваться YAML целиком: иначе yaml.Unmarshal
	// домешивает группы к дефолтным (merge), и в конфиге появляется лишняя
	// дефолтная группа. Сбрасываем перед разбором и восстанавливаем,
	// только если в YAML группы не заданы вовсе.
	cfg.Groups = nil
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(cfg.Groups) == 0 {
		cfg.Groups = Default().Groups
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save записывает конфигурацию в файл (атомарно).
func (c *Config) Save(path string) error {
	c.Normalize()
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// YAML возвращает сериализованную конфигурацию (для веб-интерфейса).
func (c *Config) YAML() string {
	c.Normalize()
	data, _ := yaml.Marshal(c)
	return string(data)
}

// Clone возвращает глубокую копию конфигурации. Нужна для безопасного
// read-modify-write при изменении блоклиста через API: исходный снимок,
// который читает DNS-сервер, не должен меняться под ним.
func (c *Config) Clone() *Config {
	cp := *c
	cp.Block.Domains = append([]string(nil), c.Block.Domains...)
	cp.Rules = append([]Rule(nil), c.Rules...)
	cp.Groups = make(map[string]Group, len(c.Groups))
	for name, g := range c.Groups {
		ng := g
		ng.Primary = append([]string(nil), g.Primary...)
		ng.Fallback = append([]string(nil), g.Fallback...)
		ng.Bootstrap = append([]string(nil), g.Bootstrap...)
		if g.Hosts != nil {
			ng.Hosts = make(map[string]string, len(g.Hosts))
			for h, ip := range g.Hosts {
				ng.Hosts[h] = ip
			}
		}
		if g.ECS != nil {
			v := *g.ECS
			ng.ECS = &v
		}
		cp.Groups[name] = ng
	}
	return &cp
}

// Normalize приводит конфигурацию к каноническому виду: заполняет значения по
// умолчанию и нормализует имена (домены блоклиста, хосты, протоколы).
func (c *Config) Normalize() {
	if c.Listen.UDP == "" {
		c.Listen.UDP = ":53"
	}
	if c.Listen.TCP == "" {
		c.Listen.TCP = ":53"
	}
	if c.Web.Listen == "" {
		c.Web.Listen = ":80"
	}
	c.ECS.Subnet = strings.TrimSpace(c.ECS.Subnet)
	c.ECS.IPv6Subnet = strings.TrimSpace(c.ECS.IPv6Subnet)
	if c.Cache.MaxEntries <= 0 {
		c.Cache.MaxEntries = 10000
	}
	if c.Stats.MaxDomains <= 0 {
		c.Stats.MaxDomains = 50000
	}
	c.Block.Mode = strings.ToLower(strings.TrimSpace(c.Block.Mode))
	if len(c.Block.Domains) > 0 {
		seen := make(map[string]struct{}, len(c.Block.Domains))
		out := make([]string, 0, len(c.Block.Domains))
		for _, d := range c.Block.Domains {
			name := NormalizeDomain(d)
			if name == "" {
				// Пустая запись остаётся, чтобы Validate вернул ошибку, а не
				// молча выбросил опечатку в конфиге.
				out = append(out, name)
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
		sort.Strings(out)
		c.Block.Domains = out
	}
	if c.Groups == nil {
		c.Groups = map[string]Group{}
	}
	for name, g := range c.Groups {
		g.Protocol = strings.ToLower(strings.TrimSpace(g.Protocol))
		if g.Protocol == "" {
			g.Protocol = "udp"
		}
		if len(g.Hosts) > 0 {
			hosts := make(map[string]string, len(g.Hosts))
			for h, ip := range g.Hosts {
				hosts[strings.ToLower(strings.TrimSpace(h))] = strings.TrimSpace(ip)
			}
			g.Hosts = hosts
		}
		c.Groups[name] = g
	}
	if c.DefaultGroup == "" {
		c.DefaultGroup = "default"
	}
}

// Validate проверяет конфигурацию на согласованность.
func (c *Config) Validate() error {
	if len(c.Groups) == 0 {
		return fmt.Errorf("no upstream groups configured")
	}
	if _, ok := c.Groups[c.DefaultGroup]; !ok {
		return fmt.Errorf("default_group %q not found in groups", c.DefaultGroup)
	}
	for name, g := range c.Groups {
		switch g.Protocol {
		case "udp", "dot", "doh":
		default:
			return fmt.Errorf("group %q: unknown protocol %q (want udp|dot|doh)", name, g.Protocol)
		}
		if len(g.Primary) == 0 && len(g.Fallback) == 0 {
			return fmt.Errorf("group %q: no upstream servers configured", name)
		}
		for _, a := range append(append([]string{}, g.Primary...), g.Fallback...) {
			if _, _, err := SplitProtocol(a); err != nil {
				return fmt.Errorf("group %q: %w", name, err)
			}
		}
		for h, ip := range g.Hosts {
			if strings.TrimSpace(h) == "" {
				return fmt.Errorf("group %q: hosts contains empty host name", name)
			}
			if net.ParseIP(strings.Trim(strings.TrimSpace(ip), "[]")) == nil {
				return fmt.Errorf("group %q: hosts[%q] is not an IP address: %q", name, h, ip)
			}
		}
	}
	for i, r := range c.Rules {
		if strings.TrimSpace(r.Zone) == "" {
			return fmt.Errorf("rule %d: empty zone", i)
		}
		if _, ok := c.Groups[r.Group]; !ok {
			return fmt.Errorf("rule %d (zone %q): group %q not found", i, r.Zone, r.Group)
		}
	}
	if err := c.validateBlock(); err != nil {
		return err
	}
	if err := c.validateECS(); err != nil {
		return err
	}
	if c.Cache.Enabled && c.Cache.MaxTTL > 0 && c.Cache.MinTTL > c.Cache.MaxTTL {
		return fmt.Errorf("cache: min_ttl (%d) > max_ttl (%d)", c.Cache.MinTTL, c.Cache.MaxTTL)
	}
	return nil
}

// validateBlock проверяет настройки блокировки доменов.
func (c *Config) validateBlock() error {
	switch c.Block.Mode {
	case "", BlockModeNoData, BlockModeZero:
	default:
		return fmt.Errorf("block.mode: unknown mode %q (want nodata|zero)", c.Block.Mode)
	}
	for _, d := range c.Block.Domains {
		if !validDomainName(d) {
			return fmt.Errorf("block.domains: invalid domain %q", d)
		}
	}
	return nil
}

// validateECS проверяет настройки EDNS Client Subnet.
func (c *Config) validateECS() error {
	if c.ECS.Subnet != "" {
		if _, err := ParseECSNet(c.ECS.Subnet, 4); err != nil {
			return fmt.Errorf("ecs.subnet: %w", err)
		}
	}
	if c.ECS.IPv6Subnet != "" {
		if _, err := ParseECSNet(c.ECS.IPv6Subnet, 6); err != nil {
			return fmt.Errorf("ecs.ipv6_subnet: %w", err)
		}
	}
	if !c.ECS.Enabled {
		return nil
	}
	if c.ECS.Subnet == "" && c.ECS.IPv6Subnet == "" {
		return fmt.Errorf("ecs: enabled but neither subnet nor ipv6_subnet is set")
	}
	return nil
}
