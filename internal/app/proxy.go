package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ProxyFile struct {
	Path    string `json:"path"`
	Before  []byte `json:"before"`
	After   []byte `json:"after"`
	Existed bool   `json:"existed"`
	Mode    uint32 `json:"mode"`
}
type DesktopValue struct{ Backend, Schema, Key, Before, After string }
type ProxyBackup struct {
	UID     int            `json:"uid"`
	Files   []ProxyFile    `json:"files"`
	Desktop []DesktopValue `json:"desktop"`
}

func proxyBackup(p Paths) string { return filepath.Join(p.Data, "proxy-backup.json") }
func proxyVariables(port int) map[string]string {
	http := fmt.Sprintf("http://127.0.0.1:%d", port)
	socks := fmt.Sprintf("socks5h://127.0.0.1:%d", port)
	return map[string]string{"http_proxy": http, "https_proxy": http, "all_proxy": socks, "no_proxy": "localhost,127.0.0.1,::1", "HTTP_PROXY": http, "HTTPS_PROXY": http, "ALL_PROXY": socks, "NO_PROXY": "localhost,127.0.0.1,::1"}
}
func envKey(line string) string {
	s := strings.TrimSpace(line)
	if strings.HasPrefix(s, "#") {
		return ""
	}
	key, _, ok := strings.Cut(s, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(key)
}
func managedEnvironment(before []byte, vars map[string]string) []byte {
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(before), "\n"), "\n") {
		if _, ok := vars[envKey(line)]; !ok && line != "" {
			lines = append(lines, line)
		}
	}
	keys := []string{"http_proxy", "https_proxy", "all_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"}
	for _, key := range keys {
		lines = append(lines, key+"=\""+vars[key]+"\"")
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
func restoreEnvironment(current, before, after []byte) ([]byte, bool) {
	if string(current) == string(after) {
		return before, false
	}
	old := map[string][]string{}
	written := map[string]string{}
	for _, line := range strings.Split(string(before), "\n") {
		if k := envKey(line); k != "" {
			old[k] = append(old[k], line)
		}
	}
	for _, line := range strings.Split(string(after), "\n") {
		k := envKey(line)
		if _, ok := proxyVariables(7890)[k]; ok {
			written[k] = line
		}
	}
	lines := []string{}
	conflict := false
	for _, line := range strings.Split(strings.TrimSuffix(string(current), "\n"), "\n") {
		k := envKey(line)
		want, ok := written[k]
		if !ok {
			lines = append(lines, line)
			continue
		}
		if line == want {
			lines = append(lines, old[k]...)
		} else {
			lines = append(lines, line)
			conflict = true
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), conflict
}
func originalUser() (int, string) {
	uid := os.Getuid()
	if value := os.Getenv("SUDO_UID"); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			uid = n
		}
	}
	desktop := strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP"))
	if strings.Contains(desktop, "kde") {
		return uid, "kde"
	}
	if strings.Contains(desktop, "gnome") || strings.Contains(desktop, "unity") || strings.Contains(desktop, "cinnamon") {
		return uid, "gnome"
	}
	return uid, ""
}
func desktopCommand(ctx context.Context, uid int, args ...string) (string, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", err
	}
	base := []string{"-u", u.Username, "--"}
	bus := fmt.Sprintf("/run/user/%d/bus", uid)
	if _, err = os.Stat(bus); err != nil {
		base = append(base, "dbus-run-session", "--")
	}
	base = append(base, args...)
	c := exec.CommandContext(ctx, "runuser", base...)
	c.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username, "XDG_CONFIG_HOME=" + filepath.Join(u.HomeDir, ".config"), fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid), "DBUS_SESSION_BUS_ADDRESS=unix:path=" + bus}
	var out, stderr cappedBuffer
	c.Stdout = &out
	c.Stderr = &stderr
	if err = c.Run(); err != nil {
		return "", fmt.Errorf("桌面代理命令 %s 失败: %s", args[0], Redact(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
func desktopGet(ctx context.Context, uid int, v DesktopValue) (string, error) {
	if v.Backend == "gnome" {
		return desktopCommand(ctx, uid, "gsettings", "get", v.Schema, v.Key)
	}
	return desktopCommand(ctx, uid, "kreadconfig"+v.Backend, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", v.Key, "--default", "__clashcli_unset__")
}
func desktopSet(ctx context.Context, uid int, v DesktopValue, value string) error {
	var err error
	if v.Backend == "gnome" {
		_, err = desktopCommand(ctx, uid, "gsettings", "set", v.Schema, v.Key, value)
	} else {
		args := []string{"kwriteconfig" + v.Backend, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", v.Key}
		if value == "__clashcli_unset__" {
			args = append(args, "--delete")
		} else {
			args = append(args, value)
		}
		_, err = desktopCommand(ctx, uid, args...)
	}
	return err
}
func desktopValues(s Settings) ([]DesktopValue, error) {
	if s.Desktop == "" || s.Desktop == "none" {
		return nil, nil
	}
	values := []DesktopValue{}
	if s.Desktop == "gnome" {
		for _, scheme := range []string{"http", "https", "socks"} {
			values = append(values, DesktopValue{Backend: "gnome", Schema: "org.gnome.system.proxy." + scheme, Key: "host", After: "'127.0.0.1'"}, DesktopValue{Backend: "gnome", Schema: "org.gnome.system.proxy." + scheme, Key: "port", After: strconv.Itoa(s.MixedPort)})
		}
		values = append(values, DesktopValue{Backend: "gnome", Schema: "org.gnome.system.proxy", Key: "ignore-hosts", After: "['localhost', '127.0.0.0/8', '::1']"}, DesktopValue{Backend: "gnome", Schema: "org.gnome.system.proxy", Key: "mode", After: "'manual'"})
	} else if s.Desktop == "kde" {
		version := "6"
		if _, err := exec.LookPath("kwriteconfig6"); err != nil {
			version = "5"
			if _, err = exec.LookPath("kwriteconfig5"); err != nil {
				return nil, errors.New("KDE 代理需要 kwriteconfig5 或 kwriteconfig6")
			}
		}
		for _, scheme := range []string{"http", "https", "socks"} {
			protocol := "http"
			if scheme == "socks" {
				protocol = "socks"
			}
			values = append(values, DesktopValue{Backend: version, Key: scheme + "Proxy", After: fmt.Sprintf("%s://127.0.0.1:%d", protocol, s.MixedPort)})
		}
		values = append(values, DesktopValue{Backend: version, Key: "NoProxyFor", After: "localhost,127.0.0.1,::1"}, DesktopValue{Backend: version, Key: "ProxyType", After: "1"})
	} else {
		return nil, errors.New("desktop 仅支持 none、gnome、kde")
	}
	return values, nil
}
func notifyDesktop(ctx context.Context, b ProxyBackup) {
	for _, v := range b.Desktop {
		if v.Backend != "gnome" {
			_, _ = desktopCommand(ctx, b.UID, "dbus-send", "--session", "--type=signal", "/KIO/Scheduler", "org.kde.KIO.Scheduler.reparseSlaveConfiguration", "string:")
			return
		}
	}
}
func enableProxy(ctx context.Context, p Paths, s Settings) error {
	if !(RealCore{p, s}).Running(ctx) {
		return errors.New("内核尚未运行；请先运行 clashcli start")
	}
	var backup ProxyBackup
	if err := readJSON(proxyBackup(p), &backup); err == nil {
		for _, file := range backup.Files {
			content := file.After
			if current, e := os.ReadFile(file.Path); e == nil {
				if filepath.Base(file.Path) == "environment" {
					content = managedEnvironment(current, proxyVariables(s.MixedPort))
				} else if string(current) != string(file.After) && string(current) != string(file.Before) {
					return errors.New("登录脚本已被修改；请先 proxy off 保留外部修改，再重试")
				}
			}
			if err = atomicWrite(file.Path, content, os.FileMode(file.Mode)); err != nil {
				return err
			}
		}
		for _, v := range backup.Desktop {
			if err = desktopSet(ctx, backup.UID, v, v.After); err != nil {
				return err
			}
		}
		notifyDesktop(ctx, backup)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	values, err := desktopValues(s)
	if err != nil {
		return err
	}
	backup = ProxyBackup{UID: s.DesktopUID, Desktop: values}
	for i, v := range backup.Desktop {
		value, err := desktopGet(ctx, backup.UID, v)
		if err != nil {
			return err
		}
		backup.Desktop[i].Before = value
	}
	envPath := filepath.Join(p.Root, "etc/environment")
	before, err := os.ReadFile(envPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	mode := os.FileMode(0644)
	if info, e := os.Stat(envPath); e == nil {
		mode = info.Mode().Perm()
	}
	backup.Files = append(backup.Files, ProxyFile{Path: envPath, Before: before, After: managedEnvironment(before, proxyVariables(s.MixedPort)), Existed: err == nil, Mode: uint32(mode)})
	profile := filepath.Join(p.Root, "etc/profile.d/clashcli.sh")
	if err = os.MkdirAll(filepath.Dir(profile), 0755); err != nil {
		return err
	}
	before, err = os.ReadFile(profile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		return errors.New("clashcli.sh 已存在但缺少恢复记录；拒绝覆盖")
	}
	lines := []string{"# Managed by clashcli. Restored by clashcli proxy off."}
	for _, key := range []string{"http_proxy", "https_proxy", "all_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"} {
		lines = append(lines, "export "+key+"='"+proxyVariables(s.MixedPort)[key]+"'")
	}
	backup.Files = append(backup.Files, ProxyFile{Path: profile, After: []byte(strings.Join(lines, "\n") + "\n"), Mode: 0644})
	if err = writeJSON(proxyBackup(p), backup); err != nil {
		return err
	}
	fail := func(err error) error {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if restoreErr := disableProxy(cleanup, p); restoreErr != nil {
			return fmt.Errorf("%w；恢复失败: %v", err, restoreErr)
		}
		return err
	}
	for _, file := range backup.Files {
		if err = atomicWrite(file.Path, file.After, os.FileMode(file.Mode)); err != nil {
			return fail(err)
		}
	}
	for _, v := range backup.Desktop {
		if err = desktopSet(ctx, backup.UID, v, v.After); err != nil {
			return fail(err)
		}
	}
	notifyDesktop(ctx, backup)
	return nil
}
func disableProxy(ctx context.Context, p Paths) error {
	var backup ProxyBackup
	if err := readJSON(proxyBackup(p), &backup); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, file := range backup.Files {
		current, err := os.ReadFile(file.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if filepath.Base(file.Path) == "environment" {
			restored, conflict := restoreEnvironment(current, file.Before, file.After)
			if conflict {
				fmt.Fprintln(os.Stderr, "保留用户随后修改的系统代理环境变量")
			}
			if !file.Existed && string(restored) == "" {
				err = os.Remove(file.Path)
			} else {
				err = atomicWrite(file.Path, restored, os.FileMode(file.Mode))
			}
			if err != nil {
				return err
			}
		} else if string(current) == string(file.After) {
			if file.Existed {
				err = atomicWrite(file.Path, file.Before, os.FileMode(file.Mode))
			} else {
				err = os.Remove(file.Path)
			}
			if err != nil {
				return err
			}
		} else {
			fmt.Fprintln(os.Stderr, "保留用户修改的 clashcli 登录脚本")
		}
	}
	for i := len(backup.Desktop) - 1; i >= 0; i-- {
		v := backup.Desktop[i]
		current, err := desktopGet(ctx, backup.UID, v)
		if err != nil {
			return err
		}
		if current == v.After {
			if err = desktopSet(ctx, backup.UID, v, v.Before); err != nil {
				return err
			}
		}
	}
	notifyDesktop(ctx, backup)
	if err := os.Remove(proxyBackup(p)); err != nil {
		return err
	}
	return syncDir(p.Data)
}
func changeProxy(ctx context.Context, p Paths, enabled bool) error {
	unlock, err := lock(ctx, filepath.Join(p.Run, "proxy.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	if enabled {
		err = enableProxy(ctx, p, s)
	} else {
		err = disableProxy(ctx, p)
	}
	if err != nil {
		return err
	}
	return mutateSettings(ctx, p, func(current *Settings) error { current.Proxy = enabled; return nil })
}
func proxyActual(ctx context.Context, p Paths) (map[string]any, error) {
	var b ProxyBackup
	if err := readJSON(proxyBackup(p), &b); errors.Is(err, os.ErrNotExist) {
		return map[string]any{"managed": false}, nil
	} else if err != nil {
		return nil, err
	}
	result := map[string]any{"managed": true, "environment": false, "desktop": "none"}
	for _, f := range b.Files {
		current, err := os.ReadFile(f.Path)
		if err == nil && filepath.Base(f.Path) == "environment" {
			result["environment"] = string(current) == string(f.After)
		}
	}
	if len(b.Desktop) > 0 {
		valid := true
		for _, v := range b.Desktop {
			value, err := desktopGet(ctx, b.UID, v)
			if err != nil || value != v.After {
				valid = false
			}
		}
		result["desktop"] = valid
	}
	return result, nil
}
