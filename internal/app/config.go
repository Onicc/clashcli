package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Document = map[string]any
type Generation struct {
	Path              string         `json:"path"`
	SourceID          string         `json:"source_id"`
	Format            string         `json:"format"`
	URICount          int            `json:"uri_count"`
	ProviderURICounts map[string]int `json:"provider_uri_counts,omitempty"`
	Expected          Expectation    `json:"expected"`
	Committed         bool           `json:"committed"`
}
type Expectation struct {
	Proxies        []string       `json:"proxies"`
	ProxyProviders map[string]int `json:"proxy_providers"`
	RuleProviders  map[string]int `json:"rule_providers"`
	Rules          int            `json:"rules"`
}

var allowedSections = map[string]bool{"proxies": true, "proxy-groups": true, "proxy-providers": true, "rules": true, "rule-providers": true, "sub-rules": true, "dns": true, "hosts": true, "sniffer": true, "unified-delay": true, "tcp-concurrent": true}
var uriSchemes = map[string]bool{"ss": true, "ssr": true, "vmess": true, "vless": true, "trojan": true, "hysteria": true, "hysteria2": true, "hy2": true, "hysteria2+realm": true, "hy2+realm": true, "tuic": true, "socks": true, "socks5": true, "socks5h": true, "http": true, "https": true, "anytls": true, "mierus": true}

func parseSubscription(b []byte) (Document, string, int, error) {
	b = []byte(strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff")))
	if len(b) == 0 || len(b) > maxSubscription {
		return nil, "", 0, errors.New("订阅为空或过大")
	}
	var doc Document
	if err := yaml.Unmarshal(b, &doc); err == nil && doc != nil {
		if _, ok := doc["proxies"]; !ok {
			if _, ok = doc["proxy-providers"]; !ok {
				return nil, "", 0, errors.New("YAML 订阅缺少 proxies 或 proxy-providers")
			}
		}
		proxies, _ := doc["proxies"].([]any)
		providers, _ := doc["proxy-providers"].(map[string]any)
		if len(proxies) == 0 && len(providers) == 0 {
			return nil, "", 0, errors.New("订阅没有节点或 provider")
		}
		return doc, "yaml", 0, nil
	}
	text := string(b)
	format := "uri"
	if !strings.Contains(text, "://") {
		compact := strings.Join(strings.Fields(text), "")
		ok := false
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if decoded, err := enc.DecodeString(compact); err == nil {
				text = string(decoded)
				ok = true
				break
			}
		}
		if !ok {
			return nil, "", 0, errors.New("订阅不是有效的 YAML、Base64 或分享链接列表")
		}
		format = "base64"
	}
	count := 0
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		scheme, _, ok := strings.Cut(line, "://")
		if !ok || !uriSchemes[strings.ToLower(scheme)] {
			return nil, "", 0, fmt.Errorf("订阅第 %d 行不是受支持的分享链接", i+1)
		}
		count++
	}
	if count == 0 {
		return nil, "", 0, errors.New("订阅没有有效节点")
	}
	return Document{}, format, count, nil
}
func normalizedURIs(b []byte, format string) []byte {
	s := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
	if format == "base64" {
		compact := strings.Join(strings.Fields(s), "")
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if decoded, err := enc.DecodeString(compact); err == nil {
				s = string(decoded)
				break
			}
		}
	}
	lines := []string{}
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
func nodeDefaults(provider string) Document {
	return Document{
		"proxy-groups": []any{Document{"name": "PROXY", "type": "select", "proxies": []any{"AUTO"}, "use": []any{provider}}, Document{"name": "AUTO", "type": "url-test", "url": "https://www.gstatic.com/generate_204", "interval": 300, "tolerance": 50, "lazy": true, "use": []any{provider}}},
		"rules":        []any{"GEOSITE,private,DIRECT", "GEOIP,private,DIRECT,no-resolve", "GEOSITE,cn,DIRECT", "GEOIP,CN,DIRECT,no-resolve", "MATCH,PROXY"},
		"dns":          Document{"enable": true, "enhanced-mode": "fake-ip", "fake-ip-filter": []any{"*.lan", "*.local", "localhost.ptlogin2.qq.com"}, "default-nameserver": []any{"223.5.5.5", "119.29.29.29"}, "proxy-server-nameserver": []any{"https://dns.alidns.com/dns-query", "https://doh.pub/dns-query"}, "nameserver-policy": Document{"geosite:private,cn": []any{"https://dns.alidns.com/dns-query", "https://doh.pub/dns-query"}}, "nameserver": []any{"https://1.1.1.1/dns-query#PROXY", "https://8.8.8.8/dns-query#PROXY"}},
	}
}
func applyManaged(doc Document, p Paths, s Settings) {
	doc["mixed-port"] = s.MixedPort
	doc["port"] = 0
	doc["socks-port"] = 0
	doc["redir-port"] = 0
	doc["tproxy-port"] = 0
	doc["allow-lan"] = false
	doc["bind-address"] = "127.0.0.1"
	doc["mode"] = "rule"
	doc["log-level"] = "info"
	doc["ipv6"] = true
	doc["external-controller"] = fmt.Sprintf("127.0.0.1:%d", s.ControllerPort)
	doc["secret"] = s.Secret
	doc["external-ui"] = filepath.Join(p.Data, "ui/current")
	doc["external-ui-url"] = ""
	doc["external-controller-cors"] = Document{"allow-origins": []any{fmt.Sprintf("http://127.0.0.1:%d", s.ControllerPort), fmt.Sprintf("http://localhost:%d", s.ControllerPort)}, "allow-private-network": false}
	doc["geodata-mode"] = true
	doc["geo-auto-update"] = false
	doc["profile"] = Document{"store-selected": true, "store-fake-ip": true}
	doc["tun"] = Document{"enable": s.Tun, "device": "clashcli0", "stack": "mixed", "auto-route": true, "auto-redirect": true, "auto-detect-interface": true, "strict-route": true, "dns-hijack": []any{"any:53", "tcp://any:53"}, "inet6-address": []any{"fdfe:dcba:9876::1/126"}}
	dns, _ := doc["dns"].(map[string]any)
	if dns == nil {
		dns = Document{"nameserver": []any{"https://dns.alidns.com/dns-query", "https://doh.pub/dns-query"}, "default-nameserver": []any{"223.5.5.5", "119.29.29.29"}, "enhanced-mode": "fake-ip"}
	}
	dns["enable"] = true
	dns["listen"] = "127.0.0.1:1053"
	dns["ipv6"] = true
	doc["dns"] = dns
}
func buildGeneration(ctx context.Context, p Paths, s Settings, sub Subscription, raw []byte) (Generation, error) {
	var g Generation
	doc, format, count, err := parseSubscription(raw)
	if err != nil {
		return g, err
	}
	dir, err := os.MkdirTemp(filepath.Join(p.Data, "generations"), "gen-")
	if err != nil {
		return g, err
	}
	success := false
	defer func() {
		if !success {
			_ = removeGeneration(p, dir)
		}
	}()
	g = Generation{Path: dir, SourceID: sub.ID, Format: format, URICount: count, ProviderURICounts: map[string]int{}}
	if err = atomicWrite(filepath.Join(dir, "source"), raw, 0600); err != nil {
		return g, err
	}
	if format != "yaml" {
		doc = nodeDefaults("subscription")
		nodes := filepath.Join(dir, "nodes.txt")
		if err = atomicWrite(nodes, normalizedURIs(raw, format), 0600); err != nil {
			return g, err
		}
		doc["proxy-providers"] = Document{"subscription": Document{"type": "file", "path": nodes}}
	} else {
		filtered := Document{}
		for k, v := range doc {
			if allowedSections[k] {
				filtered[k] = v
			}
		}
		if err = rejectLocalCredentials(filtered); err != nil {
			return g, err
		}
		doc = filtered
		for _, section := range []string{"proxy-providers", "rule-providers"} {
			if err = materializeProviders(ctx, doc, section, dir, g.ProviderURICounts); err != nil {
				return g, err
			}
		}
		groups, _ := doc["proxy-groups"].([]any)
		rules, _ := doc["rules"].([]any)
		if len(groups) == 0 || len(rules) == 0 {
			defaults := nodeDefaults("subscription")
			if len(groups) == 0 {
				proxies, _ := doc["proxies"].([]any)
				names := []any{}
				for _, entry := range proxies {
					if proxy, ok := entry.(map[string]any); ok {
						if name, ok := proxy["name"].(string); ok {
							names = append(names, name)
						}
					}
				}
				providers := []any{}
				if pp, ok := doc["proxy-providers"].(map[string]any); ok {
					keys := sortedKeys(pp)
					for _, k := range keys {
						providers = append(providers, k)
					}
				}
				for _, entry := range defaults["proxy-groups"].([]any) {
					group := entry.(map[string]any)
					group["use"] = providers
					group["proxies"] = names
					if group["name"] == "PROXY" {
						group["proxies"] = append([]any{"AUTO"}, names...)
					}
				}
				doc["proxy-groups"] = defaults["proxy-groups"]
			}
			if len(rules) == 0 {
				target := "PROXY"
				if len(groups) > 0 {
					if group, ok := groups[0].(map[string]any); ok {
						target, _ = group["name"].(string)
					}
				}
				rr := defaults["rules"].([]any)
				rr[len(rr)-1] = "MATCH," + target
				doc["rules"] = rr
			}
		}
	}
	applyManaged(doc, p, s)
	b, err := yaml.Marshal(doc)
	if err != nil {
		return g, err
	}
	if err = atomicWrite(filepath.Join(dir, "config.yaml"), b, 0600); err != nil {
		return g, err
	}
	if err = writeJSON(filepath.Join(dir, "generation.json"), g); err != nil {
		return g, err
	}
	success = true
	return g, nil
}

// Remote proxy definitions may contain inline keys, but must not read root's local credentials.
func rejectLocalCredentials(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for key, raw := range v {
			if key == "private-key-path" || key == "certificate-path" || key == "ca-file" || key == "ca-path" {
				return errors.New("远程订阅不能引用本机证书或私钥文件")
			}
			if key == "private-key" || key == "certificate" {
				str, ok := raw.(string)
				if ok && str != "" && !strings.Contains(str, "-----BEGIN ") {
					decoded, e := base64.StdEncoding.DecodeString(str)
					if key != "private-key" || e != nil || len(decoded) != 32 {
						return errors.New("远程订阅的证书或私钥必须内嵌，不能引用本机文件")
					}
				}
			}
			if err := rejectLocalCredentials(raw); err != nil {
				return err
			}
		}
	case []any:
		for _, raw := range v {
			if err := rejectLocalCredentials(raw); err != nil {
				return err
			}
		}
	}
	return nil
}
func materializeProviders(ctx context.Context, doc Document, section, dir string, counts map[string]int) error {
	value, exists := doc[section]
	if !exists {
		return nil
	}
	providers, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s 必须为映射", section)
	}
	for _, name := range sortedKeys(providers) {
		provider, ok := providers[name].(map[string]any)
		if !ok {
			return fmt.Errorf("%s provider 格式错误", section)
		}
		typeName, _ := provider["type"].(string)
		if typeName == "inline" {
			continue
		}
		if typeName != "http" {
			return errors.New("远程订阅中的 provider 必须使用 http 或 inline 类型")
		}
		u, _ := provider["url"].(string)
		headers := http.Header{}
		if fields, ok := provider["header"].(map[string]any); ok {
			for key, raw := range fields {
				switch values := raw.(type) {
				case string:
					headers.Add(key, values)
				case []any:
					for _, value := range values {
						str, ok := value.(string)
						if !ok {
							return errors.New("provider header 必须为字符串列表")
						}
						headers.Add(key, str)
					}
				default:
					return errors.New("provider header 必须为字符串列表")
				}
			}
		}
		d, err := downloadHeaders(ctx, u, maxSubscription, "", "", headers)
		if err != nil {
			return fmt.Errorf("%s 依赖下载失败: %w", section, err)
		}
		if section == "proxy-providers" {
			nodes, format, count, e := parseSubscription(d.Body)
			if e != nil {
				return fmt.Errorf("节点 provider 无效: %w", e)
			}
			if e = rejectLocalCredentials(nodes); e != nil {
				return e
			}
			if format != "yaml" {
				counts[name] = count
				d.Body = normalizedURIs(d.Body, format)
			}
		}
		// mihomo's streaming rule parser expects block YAML, not flow-style payloads.
		if section == "rule-providers" && (provider["format"] == nil || provider["format"] == "yaml") {
			var rules struct {
				Payload []string `yaml:"payload"`
				Rules   []string `yaml:"rules"`
			}
			if err = yaml.Unmarshal(d.Body, &rules); err != nil {
				return errors.New("规则 provider YAML 无效")
			}
			if len(rules.Payload) == 0 {
				rules.Payload = rules.Rules
			}
			if len(rules.Payload) == 0 {
				return errors.New("规则 provider payload 为空")
			}
			d.Body, err = yaml.Marshal(map[string]any{"payload": rules.Payload})
			if err != nil {
				return err
			}
		}
		hash := sha256.Sum256([]byte(section + "\x00" + name))
		path := filepath.Join(dir, fmt.Sprintf("provider-%x", hash[:8]))
		if err = atomicWrite(path, d.Body, 0600); err != nil {
			return err
		}
		provider["type"] = "file"
		provider["path"] = path
		for _, k := range []string{"url", "interval", "proxy", "header", "size-limit"} {
			delete(provider, k)
		}
	}
	return nil
}
func cloneGeneration(p Paths, s Settings, from string) (Generation, error) {
	var old Generation
	if !inside(filepath.Join(p.Data, "generations"), from) {
		return old, errors.New("版本路径无效")
	}
	if err := readJSON(filepath.Join(from, "generation.json"), &old); err != nil {
		return old, err
	}
	dir, err := os.MkdirTemp(filepath.Join(p.Data, "generations"), "gen-")
	if err != nil {
		return old, err
	}
	success := false
	defer func() {
		if !success {
			_ = removeGeneration(p, dir)
		}
	}()
	entries, err := os.ReadDir(from)
	if err != nil {
		return old, err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if err = copyFile(filepath.Join(from, entry.Name()), filepath.Join(dir, entry.Name()), 0600); err != nil {
			return old, err
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return old, err
	}
	var doc Document
	if err = yaml.Unmarshal(b, &doc); err != nil {
		return old, err
	}
	for _, section := range []string{"proxy-providers", "rule-providers"} {
		if providers, ok := doc[section].(map[string]any); ok {
			for _, v := range providers {
				if provider, ok := v.(map[string]any); ok {
					path, _ := provider["path"].(string)
					if inside(from, path) {
						provider["path"] = filepath.Join(dir, filepath.Base(path))
					}
				}
			}
		}
	}
	applyManaged(doc, p, s)
	b, err = yaml.Marshal(doc)
	if err != nil {
		return old, err
	}
	if err = atomicWrite(filepath.Join(dir, "config.yaml"), b, 0600); err != nil {
		return old, err
	}
	old.Path = dir
	old.Committed = false
	if err = writeJSON(filepath.Join(dir, "generation.json"), old); err != nil {
		return old, err
	}
	success = true
	return old, nil
}

func sameGeneration(a, b string) bool {
	entriesA, err := os.ReadDir(a)
	if err != nil {
		return false
	}
	entriesB, err := os.ReadDir(b)
	if err != nil || len(entriesA) != len(entriesB) {
		return false
	}
	for _, entry := range entriesA {
		if entry.Name() == "generation.json" {
			continue
		}
		left, e1 := os.ReadFile(filepath.Join(a, entry.Name()))
		right, e2 := os.ReadFile(filepath.Join(b, entry.Name()))
		if e1 != nil || e2 != nil {
			return false
		}
		if entry.Name() == "config.yaml" {
			left = []byte(strings.ReplaceAll(string(left), a, "<GENERATION>"))
			right = []byte(strings.ReplaceAll(string(right), b, "<GENERATION>"))
		}
		if sha256.Sum256(left) != sha256.Sum256(right) {
			return false
		}
	}
	return true
}

func markCommitted(path string) error {
	var g Generation
	if err := readJSON(filepath.Join(path, "generation.json"), &g); err != nil {
		return err
	}
	g.Committed = true
	return writeJSON(filepath.Join(path, "generation.json"), g)
}
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
