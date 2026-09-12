package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

type API struct{ URL, Secret, Socket string }

func APIFor(s Settings) API {
	return API{URL: fmt.Sprintf("http://127.0.0.1:%d", s.ControllerPort), Secret: s.Secret}
}
func (a API) Do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	client := httpClient(30 * time.Second)
	if a.Socket != "" {
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", a.Socket)
		}}
	}
	defer client.CloseIdleConnections()
	address := a.URL
	if address == "" {
		address = "http://localhost"
	}
	req, err := http.NewRequestWithContext(ctx, method, address+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("内核 API 不可用: %s", Redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("内核 API %s 返回 HTTP %d", path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
	}
	return nil
}
func (a API) Snapshot(ctx context.Context) (Expectation, error) {
	e := Expectation{ProxyProviders: map[string]int{}, RuleProviders: map[string]int{}}
	var proxies struct {
		Proxies map[string]json.RawMessage `json:"proxies"`
	}
	if err := a.Do(ctx, "GET", "/proxies", nil, &proxies); err != nil {
		return e, err
	}
	for name := range proxies.Proxies {
		e.Proxies = append(e.Proxies, name)
	}
	sort.Strings(e.Proxies)
	var pp struct {
		Providers map[string]struct {
			Vehicle string            `json:"vehicleType"`
			Proxies []json.RawMessage `json:"proxies"`
		} `json:"providers"`
	}
	if err := a.Do(ctx, "GET", "/providers/proxies", nil, &pp); err != nil {
		return e, err
	}
	for name, v := range pp.Providers {
		if v.Vehicle != "Compatible" {
			e.ProxyProviders[name] = len(v.Proxies)
			if len(v.Proxies) == 0 {
				return e, errors.New("节点 provider 尚未加载或为空")
			}
		}
	}
	var rp struct {
		Providers map[string]struct {
			RuleCount int `json:"ruleCount"`
		} `json:"providers"`
	}
	if err := a.Do(ctx, "GET", "/providers/rules", nil, &rp); err != nil {
		return e, err
	}
	for name, v := range rp.Providers {
		e.RuleProviders[name] = v.RuleCount
		if v.RuleCount == 0 {
			return e, errors.New("规则 provider 尚未加载或为空")
		}
	}
	var rules struct {
		Rules []json.RawMessage `json:"rules"`
	}
	if err := a.Do(ctx, "GET", "/rules", nil, &rules); err != nil {
		return e, err
	}
	e.Rules = len(rules.Rules)
	return e, nil
}

type Core interface {
	Running(context.Context) bool
	Validate(context.Context, *Generation) error
	Reload(context.Context, string) error
	Check(context.Context, Generation) error
	Restart(context.Context) error
}
type RealCore struct {
	P Paths
	S Settings
}

func (c RealCore) Running(ctx context.Context) bool {
	var v any
	return APIFor(c.S).Do(ctx, "GET", "/version", nil, &v) == nil
}
func (c RealCore) Reload(ctx context.Context, path string) error {
	return APIFor(c.S).Do(ctx, "PUT", "/configs?force=true", map[string]string{"path": filepath.Join(path, "config.yaml")}, nil)
}
func (c RealCore) Restart(ctx context.Context) error {
	return systemctl(ctx, "restart", "clashcli.service")
}
func (c RealCore) Check(ctx context.Context, g Generation) error {
	deadline := time.Now().Add(15 * time.Second)
	var err error
	for {
		err = c.checkOnce(ctx, g)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}
func (c RealCore) checkOnce(ctx context.Context, g Generation) error {
	api := APIFor(c.S)
	actual, err := api.Snapshot(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, g.Expected) {
		return errors.New("内核实际加载的节点或规则与候选版本不一致")
	}
	var conf struct {
		MixedPort int `json:"mixed-port"`
		Tun       struct {
			Enable bool `json:"enable"`
		} `json:"tun"`
	}
	if err = api.Do(ctx, "GET", "/configs", nil, &conf); err != nil {
		return err
	}
	if conf.MixedPort != c.S.MixedPort || conf.Tun.Enable != c.S.Tun {
		return errors.New("端口或 TUN 未按配置生效")
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", c.S.MixedPort), time.Second)
	if err != nil {
		return errors.New("混合代理端口未监听")
	}
	conn.Close()
	_, ifaceErr := net.InterfaceByName("clashcli0")
	if c.S.Tun && ifaceErr != nil {
		return errors.New("TUN 网卡未创建")
	}
	if !c.S.Tun && ifaceErr == nil {
		return errors.New("关闭 TUN 后网卡仍存在")
	}
	return nil
}

type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() < 1<<20 {
		n := len(p)
		if n > (1<<20)-b.buf.Len() {
			n = (1 << 20) - b.buf.Len()
		}
		b.buf.Write(p[:n])
	}
	return len(p), nil
}
func (b *cappedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
func coreEnv(p Paths) []string {
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + p.Data, "SAFE_PATHS=" + p.Data, "TZ=UTC"}
	return env
}
func (c RealCore) Validate(ctx context.Context, g *Generation) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	probe, err := os.MkdirTemp(c.P.Run, "probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probe)
	for _, name := range []string{"geoip.dat", "GeoSite.dat", "geoip.metadb"} {
		if _, err = os.Stat(filepath.Join(c.P.Data, name)); err == nil {
			if err = os.Symlink(filepath.Join(c.P.Data, name), filepath.Join(probe, name)); err != nil {
				return err
			}
		}
	}
	config, err := os.ReadFile(filepath.Join(g.Path, "config.yaml"))
	if err != nil {
		return err
	}
	var doc Document
	if err = yaml.Unmarshal(config, &doc); err != nil {
		return err
	}
	doc["mixed-port"] = 0
	doc["external-controller"] = ""
	doc["external-controller-unix"] = filepath.Join(probe, "api.sock")
	doc["external-ui"] = ""
	doc["tun"] = Document{"enable": false}
	doc["profile"] = Document{"store-selected": false, "store-fake-ip": false}
	if dns, ok := doc["dns"].(map[string]any); ok {
		dns["listen"] = ""
	}
	if groups, ok := doc["proxy-groups"].([]any); ok {
		for _, entry := range groups {
			if group, ok := entry.(map[string]any); ok {
				if group["type"] == "url-test" || group["type"] == "fallback" || group["type"] == "load-balance" {
					group["type"] = "select"
				}
				delete(group, "url")
				delete(group, "interval")
			}
		}
	}
	if providers, ok := doc["proxy-providers"].(map[string]any); ok {
		for _, entry := range providers {
			if provider, ok := entry.(map[string]any); ok {
				delete(provider, "health-check")
			}
		}
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	file := filepath.Join(probe, "config.yaml")
	if err = atomicWrite(file, b, 0600); err != nil {
		return err
	}
	var output cappedBuffer
	check := exec.CommandContext(ctx, c.P.Core(), "-t", "-d", probe, "-f", file)
	check.Env = coreEnv(c.P)
	check.Stdout = &output
	check.Stderr = &output
	if err = check.Run(); err != nil {
		return fmt.Errorf("mihomo 配置预检失败: %s", safeCoreOutput(output.String()))
	}
	child := exec.CommandContext(ctx, c.P.Core(), "-d", probe, "-f", file)
	child.Env = coreEnv(c.P)
	child.Stdout = &output
	child.Stderr = &output
	if err = child.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	defer func() {
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = child.Process.Kill()
			<-done
		}
	}()
	api := API{Socket: filepath.Join(probe, "api.sock")}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("候选内核预检超时: %s", safeCoreOutput(output.String()))
		default:
		}
		expected, e := api.Snapshot(ctx)
		if e == nil {
			// The provider API excludes file-backed proxy names from /proxies, so check counts independently.
			if g.URICount > 0 && expected.ProxyProviders["subscription"] != g.URICount {
				return errors.New("分享链接解析数量不一致，拒绝静默丢弃节点")
			}
			if strings.Contains(output.String(), "initial proxy provider") && strings.Contains(output.String(), " error:") {
				return fmt.Errorf("provider 预检失败: %s", safeCoreOutput(output.String()))
			}
			g.Expected = expected
			return writeJSON(filepath.Join(g.Path, "generation.json"), g)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("候选配置依赖加载失败: %s", safeCoreOutput(output.String()))
		case <-time.After(150 * time.Millisecond):
		}
	}
}
func safeCoreOutput(s string) string {
	lines := []string{}
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "error") || strings.Contains(line, "failed") {
			lines = append(lines, Redact(line))
		}
	}
	result := strings.Join(lines, "; ")
	if len(result) > 1200 {
		result = result[:1200]
	}
	if result == "" {
		return "请检查内核兼容性与配置依赖"
	}
	return result
}
func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out cappedBuffer
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout = &out
	c.Stderr = &out
	err := c.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s 执行失败: %s", filepath.Base(name), Redact(out.String()))
	}
	return out.String(), nil
}
func systemctl(ctx context.Context, args ...string) error {
	_, err := commandOutput(ctx, "systemctl", args...)
	return err
}
