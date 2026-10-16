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

func TestParseGroupsNotMerged(t *testing.T) {
	cfg, err := Parse([]byte(`
groups:
  a:
    protocol: udp
    primary: ["192.168.1.1"]
default_group: a
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Groups) != 1 {
		t.Fatalf("groups = %d (%v), want 1 (дефолтные группы не должны домешиваться)", len(cfg.Groups), cfg.Groups)
	}
	if _, ok := cfg.Groups["default"]; ok {
		t.Fatalf("unexpected merged default group: %v", cfg.Groups)
	}
	if _, ok := cfg.Groups["a"]; !ok {
		t.Fatalf("group a missing: %v", cfg.Groups)
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
		"hosts value not an ip": `
groups:
  g1: {protocol: doh, primary: ["https://x/dns-query"], hosts: {x: not-an-ip}}
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

func TestParseHostsNormalized(t *testing.T) {
	cfg, err := Parse([]byte(`
groups:
  g1:
    protocol: doh
    primary: ["https://doh.example.test/dns-query"]
    hosts:
      "  DoH.Example.TEST ": " 157.228.153.163 "
default_group: g1
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ip, ok := cfg.Groups["g1"].Hosts["doh.example.test"]
	if !ok || ip != "157.228.153.163" {
		t.Fatalf("hosts not normalized: %+v", cfg.Groups["g1"].Hosts)
	}
}

func TestSplitProtocol(t *testing.T) {
	cases := []struct {
		addr      string
		wantProto string
		wantRest  string
		wantErr   bool
	}{
		{addr: "192.168.1.1:53", wantProto: "", wantRest: "192.168.1.1:53"},
		{addr: "https://9.9.9.9/dns-query", wantProto: "", wantRest: "https://9.9.9.9/dns-query"},
		{addr: "udp+192.168.1.1:53", wantProto: "udp", wantRest: "192.168.1.1:53"},
		{addr: "doh+https://9.9.9.9/dns-query", wantProto: "doh", wantRest: "https://9.9.9.9/dns-query"},
		{addr: "DOH+ https://9.9.9.9/dns-query", wantProto: "doh", wantRest: "https://9.9.9.9/dns-query"},
		{addr: "dot+dns.google:853", wantProto: "dot", wantRest: "dns.google:853"},
		// Внутри URL знак "+" не считается префиксом протокола.
		{addr: "https://host/dns-query?a=1+2", wantProto: "", wantRest: "https://host/dns-query?a=1+2"},
		// Неизвестный ведущий токен — ошибка, а не молчаливый адрес.
		{addr: "tcp+1.1.1.1", wantErr: true},
	}
	for _, c := range cases {
		proto, rest, err := SplitProtocol(c.addr)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got nil", c.addr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.addr, err)
			continue
		}
		if proto != c.wantProto || rest != c.wantRest {
			t.Errorf("%q: got (%q, %q), want (%q, %q)", c.addr, proto, rest, c.wantProto, c.wantRest)
		}
	}
}

func TestParseProtocolPrefixValidated(t *testing.T) {
	cfg, err := Parse([]byte(`
groups:
  g1:
    protocol: udp
    primary:  ["192.168.1.1:53"]
    fallback: ["doh+https://9.9.9.9/dns-query"]
default_group: g1
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Groups["g1"].Fallback) != 1 {
		t.Fatalf("fallback lost: %+v", cfg.Groups["g1"])
	}

	_, err = Parse([]byte(`
groups:
  g1: {protocol: udp, primary: ["tcp+1.1.1.1"]}
default_group: g1
`))
	if err == nil {
		t.Fatal("unknown protocol prefix: expected validation error")
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
