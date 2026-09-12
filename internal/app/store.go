package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

type Paths struct{ Root, Config, Data, Run, Bin, Units string }

func NewPaths(root string) Paths {
	return Paths{root, filepath.Join(root, "etc/clashcli"), filepath.Join(root, "var/lib/clashcli"), filepath.Join(root, "run/clashcli"), filepath.Join(root, "usr/local/lib/clashcli"), filepath.Join(root, "etc/systemd/system")}
}
func (p Paths) Settings() string   { return filepath.Join(p.Config, "config.yaml") }
func (p Paths) Current() string    { return filepath.Join(p.Data, "current") }
func (p Paths) Core() string       { return filepath.Join(p.Bin, "mihomo") }
func (p Paths) Executable() string { return filepath.Join(p.Root, "usr/local/bin/clashcli") }
func (p Paths) Ensure() error {
	for _, dir := range []string{p.Config, p.Data, p.Run, p.Bin, filepath.Join(p.Data, "generations"), filepath.Join(p.Data, "subscriptions")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	return nil
}

type Subscription struct {
	ID          string    `yaml:"id" json:"id"`
	Name        string    `yaml:"name" json:"name"`
	URL         string    `yaml:"url" json:"-"`
	Calendar    string    `yaml:"calendar" json:"calendar"`
	Generation  string    `yaml:"generation,omitempty" json:"-"`
	ETag        string    `yaml:"etag,omitempty" json:"-"`
	Modified    string    `yaml:"modified,omitempty" json:"-"`
	LastChecked time.Time `yaml:"last_checked,omitempty" json:"last_checked"`
	LastUpdated time.Time `yaml:"last_updated,omitempty" json:"last_updated"`
	LastError   string    `yaml:"last_error,omitempty" json:"last_error,omitempty"`
}
type Settings struct {
	Schema         int            `yaml:"schema" json:"schema"`
	Revision       uint64         `yaml:"revision" json:"revision"`
	MixedPort      int            `yaml:"mixed_port" json:"mixed_port"`
	ControllerPort int            `yaml:"controller_port" json:"controller_port"`
	Secret         string         `yaml:"secret" json:"-"`
	Proxy          bool           `yaml:"system_proxy" json:"system_proxy"`
	Tun            bool           `yaml:"tun" json:"tun"`
	Active         string         `yaml:"active_subscription" json:"active_subscription"`
	Subscriptions  []Subscription `yaml:"subscriptions" json:"subscriptions"`
	DesktopUID     int            `yaml:"desktop_uid" json:"desktop_uid"`
	Desktop        string         `yaml:"desktop,omitempty" json:"desktop,omitempty"`
}

func DefaultSettings() Settings {
	return Settings{Schema: 1, MixedPort: 7890, ControllerPort: 9090, Secret: randomID(32), DesktopUID: os.Getuid()}
}
func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func (s *Settings) Sub(name string) (*Subscription, error) {
	if name == "" {
		name = s.Active
	}
	for i := range s.Subscriptions {
		if s.Subscriptions[i].ID == name || s.Subscriptions[i].Name == name {
			return &s.Subscriptions[i], nil
		}
	}
	return nil, fmt.Errorf("未找到订阅 %q", name)
}
func loadSettings(p Paths) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(p.Settings())
	if err != nil {
		return s, fmt.Errorf("读取配置（首次使用请运行 clashcli init）: %w", err)
	}
	if err = yaml.Unmarshal(b, &s); err != nil {
		return s, errors.New("clashcli 配置格式错误")
	}
	if s.Schema != 1 {
		return s, fmt.Errorf("不支持配置版本 %d", s.Schema)
	}
	if s.MixedPort < 1024 || s.MixedPort > 65535 || s.ControllerPort < 1024 || s.ControllerPort > 65535 || s.MixedPort == s.ControllerPort || len(s.Secret) < 32 {
		return s, errors.New("端口或控制接口密钥配置无效")
	}
	return s, nil
}
func saveSettings(p Paths, s Settings) error {
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return atomicWrite(p.Settings(), b, 0600)
}
func atomicWrite(path string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".clashcli-write-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0600)
}
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func switchLink(path, target string) error {
	tmp := path + ".next-" + randomID(6)
	defer os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func lock(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func copyFile(src, dst string, mode os.FileMode) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 128<<20))
	if err != nil {
		return err
	}
	return atomicWrite(dst, b, mode)
}
func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func removeGeneration(p Paths, path string) error {
	root := filepath.Join(p.Data, "generations")
	if !inside(root, path) || filepath.Dir(path) != root {
		return errors.New("拒绝删除非版本目录")
	}
	if current, _ := os.Readlink(p.Current()); current == path {
		return errors.New("拒绝删除活动版本")
	}
	return os.RemoveAll(path)
}

var urlPattern = regexp.MustCompile(`(?i)(?:https?|socks5h?)://[^\s<>"']+`)
var credentialPattern = regexp.MustCompile(`(?i)(token|password|passwd|secret|authorization|uuid)(["']?\s*[:=]\s*["']?)[^\s,"'}]+`)
var sharePattern = regexp.MustCompile(`(?i)(?:ss|ssr|vmess|vless|trojan|hysteria2?(?:\+realm)?|hy2(?:\+realm)?|tuic|anytls|mierus)://[^\s<>"']+`)

func Redact(s string) string {
	s = sharePattern.ReplaceAllString(s, "[REDACTED SHARE LINK]")
	s = urlPattern.ReplaceAllStringFunc(s, func(v string) string {
		// Subscription credentials can be in a path or fragment, not just a query.
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			return "[REDACTED URL]"
		}
		return u.Scheme + "://" + u.Host + "/[REDACTED]"
	})
	return credentialPattern.ReplaceAllString(s, "${1}${2}[REDACTED]")
}
