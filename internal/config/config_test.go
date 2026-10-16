package config

import "testing"

func TestDefaultValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestParseNormalize(t *testing.T) {
	cfg, err := Parse([]byte(`
groups:
  g1:
    primary: ["1.1.1.1"]
default_group: g1
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Groups["g1"].Protocol != "udp" {
		t.Fatalf("protocol = %q, want udp", cfg.Groups["g1"].Protocol)
	}
	if cfg.Listen.UDP != ":53" || cfg.Web.Listen != ":8080" {
		t.Fatalf("normalize failed: %+v", cfg.Listen)
	}
	if cfg.Timeout().String() != "3s" {
		t.Fatalf("timeout = %s, want 3s", cfg.Timeout())
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown protocol": `
groups:
  g1: {protocol: tcp, primary: ["1.1.1.1"]}
default_group: g1
`,
		"missing default group": `
groups:
  g1: {primary: ["1.1.1.1"]}
default_group: nope
`,
		"rule references missing group": `
groups:
  g1: {primary: ["1.1.1.1"]}
default_group: g1
rules:
  - {zone: ru, group: missing}
`,
		"no servers": `
groups:
  g1: {protocol: udp}
default_group: g1
`,
		"min greater than max": `
groups:
  g1: {primary: ["1.1.1.1"]}
default_group: g1
cache: {enabled: true, min_ttl: 100, max_ttl: 10}
`,
	}
	for name, yaml := range cases {
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestYAMLRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Rules = []Rule{{Zone: "ru", Group: "default"}}
	yaml := cfg.YAML()
	back, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(back.Rules) != 1 || back.Rules[0].Zone != "ru" {
		t.Fatalf("round trip lost rules: %+v", back.Rules)
	}
}
