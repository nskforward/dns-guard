// Package config описывает конфигурацию dns-guard: слушатели, кэш,
// группы апстримов и правила маршрутизации по зонам.
package config

import (
	"fmt"
	"net"
	"os"
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
		Web:    Web{Enabled: true, Listen: ":8080"},
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
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save записывает конфигурацию в файл (атомарно).
func (c *Config) Save(path string) error {
	c.normalize()
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
	c.normalize()
	data, _ := yaml.Marshal(c)
	return string(data)
}

func (c *Config) normalize() {
	if c.Listen.UDP == "" {
		c.Listen.UDP = ":53"
	}
	if c.Listen.TCP == "" {
		c.Listen.TCP = ":53"
	}
	if c.Web.Listen == "" {
		c.Web.Listen = ":8080"
	}
	if c.Cache.MaxEntries <= 0 {
		c.Cache.MaxEntries = 10000
	}
	if c.Stats.MaxDomains <= 0 {
		c.Stats.MaxDomains = 50000
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
	if c.Cache.Enabled && c.Cache.MaxTTL > 0 && c.Cache.MinTTL > c.Cache.MaxTTL {
		return fmt.Errorf("cache: min_ttl (%d) > max_ttl (%d)", c.Cache.MinTTL, c.Cache.MaxTTL)
	}
	return nil
}
