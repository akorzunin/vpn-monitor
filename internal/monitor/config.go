package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen      string         `yaml:"listen"`
	APITokenEnv string         `yaml:"api_token_env"`
	SingBox     string         `yaml:"sing_box"`
	WebDir      string         `yaml:"web_dir"`
	HistoryFile string         `yaml:"history_file"`
	Interval    time.Duration  `yaml:"interval"`
	Timeout     time.Duration  `yaml:"timeout"`
	Samples     int            `yaml:"samples"`
	Targets     []Target       `yaml:"targets"`
	Controls    []Target       `yaml:"controls"`
	Speed       SpeedConfig    `yaml:"speed"`
	UDP         UDPConfig      `yaml:"udp"`
	Telegram    TelegramConfig `yaml:"telegram"`
	Servers     []Server       `yaml:"servers"`
	Token       string         `yaml:"-"`
	Routes      []Route        `yaml:"-"`
}
type Target struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	Status int    `yaml:"status"`
}
type SpeedConfig struct {
	URL      string        `yaml:"url"`
	MaxBytes int64         `yaml:"max_bytes"`
	Timeout  time.Duration `yaml:"timeout"`
}
type UDPConfig struct {
	Endpoint string        `yaml:"endpoint"`
	TokenEnv string        `yaml:"token_env"`
	Samples  int           `yaml:"samples"`
	Timeout  time.Duration `yaml:"timeout"`
	Token    string        `yaml:"-"`
}
type TelegramConfig struct {
	TokenEnv string  `yaml:"token_env"`
	ChatIDs  []int64 `yaml:"chat_ids"`
	Token    string  `yaml:"-"`
}
type Server struct {
	Name      string     `yaml:"name"`
	Protocols []Protocol `yaml:"protocols"`
}
type Protocol struct {
	Name     string         `yaml:"name"`
	UDP      bool           `yaml:"udp"`
	Outbound map[string]any `yaml:"outbound"`
}
type Route struct {
	ID       string         `json:"id"`
	Server   string         `json:"server"`
	Protocol string         `json:"protocol"`
	UDP      bool           `json:"udp"`
	Outbound map[string]any `json:"-"`
}

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,47}$`)

func LoadConfig(path string) (Config, error) {
	c := Config{Listen: "127.0.0.1:8080", APITokenEnv: "VPN_MONITOR_API_TOKEN", SingBox: "sing-box", WebDir: "web/dist", HistoryFile: "data/history.json", Interval: time.Minute, Timeout: 10 * time.Second, Samples: 3,
		Speed: SpeedConfig{MaxBytes: 16 << 20, Timeout: 15 * time.Second}, UDP: UDPConfig{Samples: 10, Timeout: time.Second}}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	// Expand scalar values after parsing, so quotes/newlines in secrets cannot inject YAML.
	var document yaml.Node
	parser := yaml.NewDecoder(strings.NewReader(string(b)))
	if parser.Decode(&document) != nil {
		return c, errors.New("invalid YAML configuration")
	}
	var extraDocument any
	if parser.Decode(&extraDocument) != io.EOF {
		return c, errors.New("configuration must contain exactly one YAML document")
	}
	var missing bool
	pattern := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	var expand func(*yaml.Node)
	expand = func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
			node.Value = pattern.ReplaceAllStringFunc(node.Value, func(s string) string {
				v, ok := os.LookupEnv(s[2 : len(s)-1])
				if !ok || v == "" {
					missing = true
				}
				return v
			})
		}
		for _, child := range node.Content {
			expand(child)
		}
	}
	expand(&document)
	if missing {
		return c, errors.New("a referenced configuration environment variable is missing")
	}
	expanded, err := yaml.Marshal(&document)
	if err != nil {
		return c, errors.New("cannot encode expanded configuration")
	}
	d := yaml.NewDecoder(strings.NewReader(string(expanded)))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, errors.New("invalid YAML configuration (check field names and value types)")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("configuration must contain exactly one YAML document")
	}
	if err = c.validate(); err != nil {
		return c, err
	}
	return c, nil
}
func validHTTPS(s string) bool {
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}
func (c *Config) validate() error {
	c.Token = os.Getenv(c.APITokenEnv)
	if len(c.Token) < 24 {
		return errors.New("API token environment variable must contain at least 24 characters")
	}
	if _, _, e := net.SplitHostPort(c.Listen); e != nil {
		return errors.New("listen must be host:port")
	}
	if c.Interval < 10*time.Second || c.Interval > 24*time.Hour || c.Timeout < time.Millisecond || c.Timeout > time.Minute || c.Samples < 1 || c.Samples > 20 {
		return errors.New("invalid interval (10s–24h), timeout (1ms–1m), or samples (1–20)")
	}
	if c.HistoryFile == "" || c.SingBox == "" {
		return errors.New("history_file and sing_box are required")
	}
	if len(c.Targets) < 1 || len(c.Targets) > 8 || len(c.Controls) < 2 || len(c.Controls) > 8 {
		return errors.New("configure 1–8 targets and 2–8 independent direct controls")
	}
	for _, targets := range [][]Target{c.Targets, c.Controls} {
		names, urls := map[string]bool{}, map[string]bool{}
		for _, t := range targets {
			if !nameRE.MatchString(t.Name) || !validHTTPS(t.URL) || t.Status < 200 || t.Status >= 400 || names[t.Name] || urls[t.URL] {
				return errors.New("targets require unique names/HTTPS URLs and explicit expected status (200–399)")
			}
			names[t.Name] = true
			urls[t.URL] = true
		}
	}
	if c.Speed.URL != "" && (!validHTTPS(c.Speed.URL) || c.Speed.MaxBytes < 1 || c.Speed.MaxBytes > 64<<20 || c.Speed.Timeout < time.Second || c.Speed.Timeout > time.Minute) {
		return errors.New("speed requires HTTPS, 1–64MiB and a 1s–1m timeout")
	}
	if c.UDP.Endpoint != "" {
		h, p, e := net.SplitHostPort(c.UDP.Endpoint)
		port, portErr := strconv.Atoi(p)
		if e != nil || net.ParseIP(h) == nil || portErr != nil || port < 1 || port > 65535 {
			return errors.New("UDP endpoint must use an explicit IP:port")
		}
		c.UDP.Token = os.Getenv(c.UDP.TokenEnv)
		if len(c.UDP.Token) < 24 || c.UDP.Samples < 1 || c.UDP.Samples > 100 || c.UDP.Timeout < 10*time.Millisecond || c.UDP.Timeout > 5*time.Second {
			return errors.New("UDP requires a 24-character secret, 1–100 samples and 10ms–5s timeout")
		}
	}
	if c.Telegram.TokenEnv != "" {
		c.Telegram.Token = os.Getenv(c.Telegram.TokenEnv)
		if c.Telegram.Token == "" || len(c.Telegram.ChatIDs) == 0 {
			return errors.New("Telegram requires its token and an explicit chat allowlist")
		}
	}
	seen := map[string]bool{}
	for _, s := range c.Servers {
		if !nameRE.MatchString(s.Name) {
			return errors.New("invalid server name")
		}
		for _, p := range s.Protocols {
			id := s.Name + "/" + p.Name
			if !nameRE.MatchString(p.Name) || seen[id] {
				return errors.New("invalid or duplicate protocol name")
			}
			seen[id] = true
			typ, _ := p.Outbound["type"].(string)
			switch typ {
			case "ssh", "naive", "vless", "vmess", "trojan", "shadowsocks", "hysteria2", "hysteria", "tuic", "socks", "http":
			default:
				return fmt.Errorf("unsupported VPN outbound type for %s", id)
			}
			for _, field := range []string{"detour", "domain_resolver", "network_strategy", "bind_interface", "routing_mark"} {
				if _, ok := p.Outbound[field]; ok {
					return errors.New("outbound routing overrides are forbidden; configure standalone routes")
				}
			}
			if tls, ok := p.Outbound["tls"].(map[string]any); ok && tls["insecure"] == true {
				return errors.New("TLS verification cannot be disabled")
			}
			if typ == "ssh" {
				pins, ok := p.Outbound["host_key"].([]any)
				if !ok || len(pins) == 0 {
					return errors.New("SSH outbounds require pinned host_key values")
				}
			}
			if p.UDP && (typ == "ssh" || typ == "naive" || typ == "http") {
				return fmt.Errorf("%s does not support the UDP test", id)
			}
			// Check JSON compatibility before launching a client.
			if _, e := json.Marshal(p.Outbound); e != nil {
				return errors.New("outbound must be JSON-compatible")
			}
			c.Routes = append(c.Routes, Route{ID: id, Server: s.Name, Protocol: p.Name, UDP: p.UDP, Outbound: p.Outbound})
		}
	}
	if len(c.Routes) == 0 || len(c.Routes) > 64 {
		return errors.New("configure 1–64 routes")
	}
	return nil
}
