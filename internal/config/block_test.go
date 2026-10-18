package config

import (
	"strings"
	"testing"
)

func TestParseBlockNormalizesDomains(t *testing.T) {
	cfg, err := Parse([]byte(`
block:
  mode: ZERO
  domains: ["*.Example.COM.", "example.com", "ads.example.org"]
groups:
  a:
    protocol: udp
    primary: ["8.8.8.8"]
default_group: a
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Block.Mode != BlockModeZero {
		t.Fatalf("mode = %q, want zero", cfg.Block.Mode)
	}
	want := []string{"ads.example.org", "example.com"}
	if len(cfg.Block.Domains) != len(want) {
		t.Fatalf("domains = %v, want %v", cfg.Block.Domains, want)
	}
	for i := range want {
		if cfg.Block.Domains[i] != want[i] {
			t.Fatalf("domains = %v, want %v", cfg.Block.Domains, want)
		}
	}
	if got := cfg.Block.ModeOrDefault(); got != BlockModeZero {
		t.Fatalf("mode or default = %q", got)
	}
}

func TestParseBlockDefaultMode(t *testing.T) {
	cfg, err := Parse([]byte(`
block:
  domains: ["example.com"]
groups:
  a:
    protocol: udp
    primary: ["8.8.8.8"]
default_group: a
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Block.ModeOrDefault() != BlockModeNoData {
		t.Fatalf("default mode = %q, want nodata", cfg.Block.ModeOrDefault())
	}
}

func TestParseBlockRejectsInvalid(t *testing.T) {
	cases := []string{
		`
block: {mode: drop}
groups: {a: {protocol: udp, primary: ["8.8.8.8"]}}
default_group: a
`,
		`
block: {domains: [""]}
groups: {a: {protocol: udp, primary: ["8.8.8.8"]}}
default_group: a
`,
		`
block: {domains: ["bad domain"]}
groups: {a: {protocol: udp, primary: ["8.8.8.8"]}}
default_group: a
`,
	}
	for i, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

// TestYAMLKeepsBlockOmittedByDefault проверяет, что секция block не появляется
// в сериализованном конфиге, пока блоклист не используется.
func TestYAMLKeepsBlockOmittedByDefault(t *testing.T) {
	if y := Default().YAML(); strings.Contains(y, "block:") {
		t.Fatalf("default config must not contain block section:\n%s", y)
	}
	cfg := Default()
	cfg.Block.Domains = []string{"example.com"}
	if y := cfg.YAML(); !strings.Contains(y, "example.com") {
		t.Fatalf("block section missing from YAML:\n%s", y)
	}
}

func TestCloneDoesNotAliasBlockDomains(t *testing.T) {
	cfg := Default()
	cfg.Block.Domains = []string{"a.com"}

	cp := cfg.Clone()
	cp.Block.Domains = append(cp.Block.Domains, "b.com")
	cp.Groups["default"] = Group{Protocol: "udp", Primary: []string{"9.9.9.9"}}

	if len(cfg.Block.Domains) != 1 || cfg.Block.Domains[0] != "a.com" {
		t.Fatalf("original blocklist mutated: %v", cfg.Block.Domains)
	}
	if cfg.Groups["default"].Primary[0] != "77.88.8.8" {
		t.Fatalf("original groups mutated: %+v", cfg.Groups["default"])
	}
}
