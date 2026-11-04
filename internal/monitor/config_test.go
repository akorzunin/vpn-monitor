package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureYAML = `
controls:
  - {name: a, url: 'https://example.com/', status: 200}
  - {name: b, url: 'https://example.org/', status: 200}
targets:
  - {name: target, url: 'https://example.net/', status: 204}
servers:
  - name: eu
    protocols:
      - name: vless
        udp: true
        outbound:
          type: vless
          server: 192.0.2.1
          server_port: 443
          uuid: '${TEST_VPN_SECRET}'
`

func loadFixture(t *testing.T, content string) (Config, error) {
	t.Helper()
	t.Setenv("VPN_MONITOR_API_TOKEN", strings.Repeat("a", 32))
	t.Setenv("TEST_VPN_SECRET", "value-with-\"quotes\"\nand: colon")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if e := os.WriteFile(path, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
	return LoadConfig(path)
}
func TestConfigDefaultsAndSafeExpansion(t *testing.T) {
	c, e := loadFixture(t, fixtureYAML)
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Routes) != 1 || c.Routes[0].ID != "eu/vless" || c.Routes[0].Outbound["uuid"] != os.Getenv("TEST_VPN_SECRET") {
		t.Fatalf("invalid expanded route: %+v", c.Routes)
	}
	if c.Samples != 3 {
		t.Fatal("default samples missing")
	}
}
func TestConfigRejectsInvalidInputs(t *testing.T) {
	cases := map[string]string{
		"unknown field":      "typo: true\n" + fixtureYAML,
		"multiple docs":      fixtureYAML + "\n---\nlisten: localhost:9999\n",
		"missing secret":     strings.ReplaceAll(fixtureYAML, "TEST_VPN_SECRET", "MISSING_VPN_MONITOR_TEST_SECRET"),
		"HTTP URL":           strings.ReplaceAll(fixtureYAML, "https://example.net/", "http://example.net/"),
		"unbounded samples":  "samples: 100000\n" + fixtureYAML,
		"too frequent":       "interval: 1ms\n" + fixtureYAML,
		"duplicate controls": strings.ReplaceAll(fixtureYAML, "https://example.org/", "https://example.com/"),
		"UDP on SSH":         strings.ReplaceAll(fixtureYAML, "type: vless", "type: ssh"),
		"direct fallback":    strings.ReplaceAll(fixtureYAML, "type: vless", "type: direct"),
		"detour":             strings.ReplaceAll(fixtureYAML, "type: vless", "type: vless\n          detour: another"),
		"bad endpoint":       "udp: {endpoint: 'example.net:9444', token_env: VPN_MONITOR_API_TOKEN}\n" + fixtureYAML,
		"unrestricted bot":   "telegram: {token_env: VPN_MONITOR_API_TOKEN}\n" + fixtureYAML,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := loadFixture(t, content); e == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}
func TestShortAPITokenRejected(t *testing.T) {
	c, e := loadFixture(t, fixtureYAML)
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("VPN_MONITOR_API_TOKEN", "short")
	if c.validate() == nil {
		t.Fatal("short token accepted")
	}
}
