package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedactionPathFragmentAndRealm(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/sub/SUPER_PRIVATE",
		"https://example.com/#SUPER_PRIVATE",
		"https://user:SUPER_PRIVATE@example.com/path",
		"https://example.com/%SUPER_PRIVATE", // malformed escaping must fail closed
		"hy2+realm://SUPER_PRIVATE@example.com:443",
		"hysteria2+realm://SUPER_PRIVATE@example.com:443",
		"socks5h://user:SUPER_PRIVATE@example.com:1080",
	} {
		if got := Redact(raw); strings.Contains(got, "SUPER_PRIVATE") {
			t.Fatalf("credential leaked: %s", got)
		}
	}
	if got := Redact("connect https://example.com/sub/private failed"); !strings.Contains(got, "example.com") || !strings.Contains(got, "failed") {
		t.Fatalf("lost useful diagnostics: %s", got)
	}
}

func TestProxyStatusIgnoresUnrelatedEnvironmentChanges(t *testing.T) {
	after := managedEnvironment([]byte("EDITOR=vi\n"), proxyVariables(7890))
	for _, current := range [][]byte{after, append(append([]byte{}, after...), []byte("OTHER=preserved\n")...)} {
		if !proxyEnvironmentMatches(current, after) {
			t.Fatal("unrelated environment change reported as broken proxy")
		}
	}
	for _, current := range []string{
		strings.Replace(string(after), "http_proxy=", "deleted=", 1),
		strings.Replace(string(after), "127.0.0.1:7890", "127.0.0.1:9999", 1),
		string(after) + "http_proxy=duplicate\n",
	} {
		if proxyEnvironmentMatches([]byte(current), after) {
			t.Fatal("accepted a missing, changed or duplicated proxy value")
		}
	}
}

func TestHealthChecksRuntimeAndProxyDrift(t *testing.T) {
	p := testPaths(t)
	s := DefaultSettings()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s.MixedPort = listener.Addr().(*net.TCPAddr).Port
	var broken atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/proxies":
			fmt.Fprint(w, `{"proxies":{"DIRECT":{}}}`)
		case "/providers/proxies", "/providers/rules":
			fmt.Fprint(w, `{"providers":{}}`)
		case "/rules":
			fmt.Fprint(w, `{"rules":[{}]}`)
		case "/configs":
			fmt.Fprintf(w, `{"mixed-port":%d,"tun":{"enable":false}}`, s.MixedPort)
		}
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	s.ControllerPort, _ = strconv.Atoi(port)
	g := Generation{Path: filepath.Join(p.Data, "generations", "gen-health"), Expected: Expectation{Proxies: []string{"DIRECT"}, ProxyProviders: map[string]int{}, RuleProviders: map[string]int{}, Rules: 1}}
	if err = os.Mkdir(g.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(g.Path, "generation.json"), g); err != nil {
		t.Fatal(err)
	}
	if err = switchLink(p.Current(), g.Path); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = checkHealth(ctx, p, s); err != nil {
		t.Fatal(err)
	}
	s.Proxy = true
	if checkHealth(ctx, p, s) == nil {
		t.Fatal("missing proxy state reported healthy")
	}
	env := ProxyFile{Path: filepath.Join(p.Config, "environment"), After: managedEnvironment(nil, proxyVariables(s.MixedPort))}
	profile := ProxyFile{Path: filepath.Join(p.Config, "clashcli.sh"), After: []byte("fixture profile")}
	for _, file := range []ProxyFile{env, profile} {
		if err = atomicWrite(file.Path, file.After, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = writeJSON(proxyBackup(p), ProxyBackup{Files: []ProxyFile{env, profile}}); err != nil {
		t.Fatal(err)
	}
	if err = checkHealth(ctx, p, s); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(profile.Path); err != nil {
		t.Fatal(err)
	}
	if checkHealth(ctx, p, s) == nil {
		t.Fatal("missing login script reported healthy")
	}
	s.Proxy = false
	if err = os.Remove(proxyBackup(p)); err != nil {
		t.Fatal(err)
	}
	broken.Store(true)
	if checkHealth(ctx, p, s) == nil {
		t.Fatal("unavailable API reported healthy")
	}
	broken.Store(false)
	listener.Close()
	if checkHealth(ctx, p, s) == nil {
		t.Fatal("closed proxy listener reported healthy")
	}
}

// Executed only by the test's journalctl subprocess, before Go flag parsing.
func init() {
	if os.Getenv("CLASHCLI_TEST_LOG_HELPER") == "1" && filepath.Base(os.Args[0]) == "journalctl" {
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 4<<20) + "\n")
		os.Exit(0)
	}
}

func TestOversizedLogLineDoesNotHang(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.Symlink(exe, filepath.Join(dir, "journalctl")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLASHCLI_TEST_LOG_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = showLogs(ctx, false, "", 100); err == nil {
		t.Fatal("oversized log line accepted")
	}
	if ctx.Err() != nil {
		t.Fatal("journalctl hung until context timeout")
	}
}
