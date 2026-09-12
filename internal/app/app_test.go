package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	p := NewPaths(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestParseSubscriptions(t *testing.T) {
	uri := "ss://YWVzLTEyOC1nY206dGVzdA@example.com:443#test\n"
	cases := []struct {
		name, body, format string
		count              int
		bad                bool
	}{
		{"yaml", "proxies:\n  - {name: test, type: ss, server: example.com, port: 443, cipher: aes-128-gcm, password: test}\n", "yaml", 0, false},
		{"uri", uri, "uri", 1, false}, {"base64", base64.StdEncoding.EncodeToString([]byte(uri)), "base64", 1, false}, {"unpadded", base64.RawURLEncoding.EncodeToString([]byte(uri)), "base64", 1, false},
		{"bom", "\ufeff" + uri, "uri", 1, false}, {"empty", "", "", 0, true}, {"empty nodes", "proxies: []", "", 0, true}, {"html", "<html>login</html>", "", 0, true}, {"unknown", "invalid://secret", "", 0, true}, {"partial", uri + "nonsense", "", 0, true}, {"broken yaml", "proxies: [", "", 0, true}, {"duplicate key", "proxies: []\nproxies: []", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, format, count, err := parseSubscription([]byte(tc.body))
			if (err != nil) != tc.bad {
				t.Fatalf("error=%v", err)
			}
			if !tc.bad && (format != tc.format || count != tc.count) {
				t.Fatalf("%s %d", format, count)
			}
		})
	}
}
func TestDownloads(t *testing.T) {
	t.Run("content type not authoritative", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.UserAgent(), "clash.meta") {
				t.Error("UA missing")
			}
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("ETag", "test-tag")
			fmt.Fprint(w, "proxies: []")
		}))
		defer server.Close()
		d, err := download(context.Background(), server.URL, 100, "", "")
		if err != nil || d.ETag != "test-tag" {
			t.Fatal(d, err)
		}
	})
	t.Run("304", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-None-Match") != "tag" {
				t.Error("etag missing")
			}
			w.WriteHeader(304)
		}))
		defer server.Close()
		d, err := download(context.Background(), server.URL, 100, "tag", "")
		if err != nil || !d.Unchanged {
			t.Fatal(d, err)
		}
	})
	for _, status := range []int{401, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var n atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1); w.WriteHeader(status) }))
			defer server.Close()
			_, err := download(context.Background(), server.URL, 100, "", "")
			if err == nil || n.Load() != 1 {
				t.Fatal(err, n.Load())
			}
		})
	}
	t.Run("limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", 101)) }))
		defer server.Close()
		_, err := download(context.Background(), server.URL, 100, "", "")
		if err == nil {
			t.Fatal("accepted oversized input")
		}
	})
	t.Run("TLS verifies", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := download(ctx, server.URL, 100, "", "")
		if err == nil {
			t.Fatal("accepted untrusted certificate")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := download(ctx, "http://127.0.0.1:1/", 100, "", "")
		if err == nil {
			t.Fatal("ignored cancellation")
		}
	})
}
func TestManagedConfigAndDependencies(t *testing.T) {
	p := testPaths(t)
	s := DefaultSettings()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "payload: ['example.com']") }))
	defer server.Close()
	raw := fmt.Sprintf(`mixed-port: 1234
external-controller: 0.0.0.0:1234
secret: evil
tun: {enable: true}
listeners: [{name: danger, port: 9999, type: http}]
proxies: [{name: test, type: socks5, server: example.com, port: 1080}]
proxy-groups: [{name: PROXY, type: select, proxies: [test]}]
rule-providers:
  domains: {type: http, behavior: domain, url: %s, path: /etc/shadow}
rules: ['RULE-SET,domains,DIRECT', 'MATCH,PROXY']
`, server.URL)
	g, err := buildGeneration(context.Background(), p, s, Subscription{ID: "test"}, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(g.Path, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err = yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["mixed-port"] != 7890 || doc["secret"] == "evil" || doc["listeners"] != nil {
		t.Fatal("remote config escaped overlay")
	}
	provider := doc["rule-providers"].(map[string]any)["domains"].(map[string]any)
	path := provider["path"].(string)
	if provider["type"] != "file" || provider["url"] != nil || !inside(g.Path, path) {
		t.Fatal(provider)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(payload), "\n    - example.com") {
		t.Fatal("rule YAML not normalized", err)
	}
	identical, err := buildGeneration(context.Background(), p, s, Subscription{ID: "test"}, []byte(raw))
	if err != nil || !sameGeneration(g.Path, identical.Path) {
		t.Fatal("equivalent content triggers needless reload", err)
	}
	s.Tun = true
	cloned, err := cloneGeneration(p, s, g.Path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(cloned.Path, "config.yaml"))
	if strings.Contains(string(b), g.Path) {
		t.Fatal("clone references old providers")
	}
}
func TestRemoteFileProviderRejected(t *testing.T) {
	p := testPaths(t)
	_, err := buildGeneration(context.Background(), p, DefaultSettings(), Subscription{}, []byte("proxy-providers:\n  malicious: {type: file, path: /etc/shadow}\n"))
	if err == nil {
		t.Fatal("allowed local file")
	}
	_, err = buildGeneration(context.Background(), p, DefaultSettings(), Subscription{}, []byte("proxies: [{name: ssh, type: ssh, server: example.com, port: 22, private-key: /root/.ssh/id_rsa}]"))
	if err == nil {
		t.Fatal("remote proxy can read local key")
	}
}

type fakeCore struct {
	running    bool
	reloadFail map[string]bool
	checkFail  map[string]bool
	loaded     string
	restarts   int
}

func (c *fakeCore) Running(context.Context) bool                { return c.running }
func (c *fakeCore) Validate(context.Context, *Generation) error { return nil }
func (c *fakeCore) Reload(_ context.Context, path string) error {
	c.loaded = path
	if c.reloadFail[path] {
		return errors.New("reload failed")
	}
	return nil
}
func (c *fakeCore) Check(_ context.Context, g Generation) error {
	if c.checkFail[g.Path] {
		return errors.New("listener failed")
	}
	return nil
}
func (c *fakeCore) Restart(context.Context) error { c.restarts++; return nil }
func txFixture(t *testing.T) (Paths, Settings, Generation, Generation, *fakeCore) {
	t.Helper()
	p := testPaths(t)
	s := DefaultSettings()
	s.Active = "a"
	s.Subscriptions = []Subscription{{ID: "a", Name: "test", URL: "https://example.com/?token=private"}}
	if err := saveSettings(p, s); err != nil {
		t.Fatal(err)
	}
	gens := []Generation{}
	for i := 0; i < 2; i++ {
		path := filepath.Join(p.Data, "generations", fmt.Sprintf("gen-%d", i))
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		g := Generation{Path: path, SourceID: "a"}
		if err := writeJSON(filepath.Join(path, "generation.json"), g); err != nil {
			t.Fatal(err)
		}
		gens = append(gens, g)
	}
	if err := switchLink(p.Current(), gens[0].Path); err != nil {
		t.Fatal(err)
	}
	core := &fakeCore{running: true, reloadFail: map[string]bool{}, checkFail: map[string]bool{}, loaded: gens[0].Path}
	return p, s, gens[0], gens[1], core
}
func TestAtomicCommitAndRollback(t *testing.T) {
	for _, failure := range []string{"", "reload", "check", "rollback"} {
		t.Run(failure, func(t *testing.T) {
			p, s, old, next, c := txFixture(t)
			s.Tun = true
			if failure == "reload" || failure == "rollback" {
				c.reloadFail[next.Path] = true
			}
			if failure == "check" {
				c.checkFail[next.Path] = true
			}
			if failure == "rollback" {
				c.reloadFail[old.Path] = true
			}
			m := Manager{P: p, Core: c}
			err := m.Apply(context.Background(), next, s, 0)
			link, _ := os.Readlink(p.Current())
			actual, _ := loadSettings(p)
			if failure == "" {
				if err != nil || link != next.Path || !actual.Tun || actual.Revision != 1 {
					t.Fatal(err, link, actual)
				}
			} else {
				if err == nil || link != old.Path || actual.Tun {
					t.Fatal(err, link, actual)
				}
				if failure == "rollback" {
					if _, err = os.Stat(m.journal()); !errors.Is(err, os.ErrNotExist) || c.restarts != 1 {
						t.Fatal("restart fallback did not finalize recovery")
					}
				}
			}
		})
	}
}
func TestCrashRecoveryEveryPhase(t *testing.T) {
	for _, phase := range []string{"prepared", "switched", "reloaded", "settings", "committed"} {
		t.Run(phase, func(t *testing.T) {
			p, s, old, next, c := txFixture(t)
			s.Tun = true
			m := Manager{P: p, Core: c, Hook: func(at string) error {
				if at == phase {
					return errors.New("power loss")
				}
				return nil
			}}
			if err := m.Apply(context.Background(), next, s, 0); err == nil {
				t.Fatal("hook did not fire")
			}
			m.Hook = nil
			if err := m.Recover(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			link, _ := os.Readlink(p.Current())
			actual, err := loadSettings(p)
			if err != nil {
				t.Fatal(err)
			}
			want := old.Path
			if phase == "committed" {
				want = next.Path
			}
			if link != want || actual.Tun != (phase == "committed") {
				t.Fatal(link, actual)
			}
			if actual.Secret == "" || actual.Subscriptions[0].URL == "" {
				t.Fatal("recovery lost credentials")
			}
		})
	}
}
func TestRevisionConflict(t *testing.T) {
	p, s, old, next, c := txFixture(t)
	s.Revision = 2
	if err := saveSettings(p, s); err != nil {
		t.Fatal(err)
	}
	err := (Manager{P: p, Core: c}).Apply(context.Background(), next, s, 0)
	link, _ := os.Readlink(p.Current())
	if err == nil || link != old.Path {
		t.Fatal(err, link)
	}
}
func TestLockCancellation(t *testing.T) {
	p := testPaths(t)
	path := filepath.Join(p.Run, "lock")
	unlock, err := lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = lock(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestProxyEnvironmentRestoration(t *testing.T) {
	before := []byte("# original\nLANG=en_US.UTF-8\nhttp_proxy=\"http://old:8080\"\nCUSTOM=keep\n")
	after := managedEnvironment(before, proxyVariables(7890))
	restored, conflict := restoreEnvironment(after, before, after)
	if conflict || !bytes.Equal(restored, before) {
		t.Fatal(string(restored))
	}
	changed := strings.Replace(string(after), "CUSTOM=keep", "CUSTOM=changed", 1)
	changed = strings.Replace(changed, "https_proxy=\"http://127.0.0.1:7890\"", "https_proxy=\"http://user:9000\"", 1)
	restored, conflict = restoreEnvironment([]byte(changed), before, after)
	if !conflict || !strings.Contains(string(restored), "CUSTOM=changed") || !strings.Contains(string(restored), "http://user:9000") || strings.Contains(string(restored), "127.0.0.1:7890") {
		t.Fatal(string(restored), conflict)
	}
}
func archive(t *testing.T, name string, kind byte, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	h := &tar.Header{Name: name, Typeflag: kind, Size: int64(len(content)), Mode: 0644}
	if kind != tar.TypeReg {
		h.Size = 0
	}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if h.Size > 0 {
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}
func TestUIExtraction(t *testing.T) {
	for _, name := range []string{"../escaped", "/absolute", "evil\\path"} {
		t.Run(name, func(t *testing.T) {
			if err := extractUI(archive(t, name, tar.TypeReg, "x"), t.TempDir()); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	if err := extractUI(archive(t, "index.html", tar.TypeSymlink, ""), t.TempDir()); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := extractUI(archive(t, "./index.html", tar.TypeReg, "<html>test</html>"), t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
func TestRedactionAndJSON(t *testing.T) {
	secret := "SUPER_PRIVATE"
	raw := "failed https://host/path?token=" + secret + " password: " + secret + " https://user:" + secret + "@host/path"
	if strings.Contains(Redact(raw), secret) {
		t.Fatal(Redact(raw))
	}
	s := DefaultSettings()
	s.Subscriptions = []Subscription{{URL: "https://host?token=" + secret}}
	b, err := json.Marshal(s)
	if err != nil || strings.Contains(string(b), secret) || strings.Contains(string(b), s.Secret) {
		t.Fatal(string(b), err)
	}
}
func TestCleanupRefusesCurrentAndOutside(t *testing.T) {
	p, _, old, _, _ := txFixture(t)
	if removeGeneration(p, p.Data) == nil || removeGeneration(p, old.Path) == nil {
		t.Fatal("unsafe cleanup accepted")
	}
}
func FuzzSubscriptionParser(f *testing.F) {
	for _, seed := range []string{"proxies: []", "vmess://abc", "ss://abc@host:443#node", "\ufeff{}"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 65536 {
			return
		}
		_, _, _, _ = parseSubscription(b)
	})
}

func TestCandidateReadiness(t *testing.T) {
	doc := Document{"proxies": []any{Document{"name": "one"}}, "proxy-providers": Document{"remote": Document{}}, "rules": []any{"MATCH,one"}}
	e := Expectation{Proxies: []string{"one"}, Rules: 1, ProxyProviders: map[string]int{}}
	if candidateReady(doc, e) {
		t.Fatal("accepted API before provider initialization")
	}
	e.ProxyProviders["remote"] = 1
	if !candidateReady(doc, e) {
		t.Fatal("ready snapshot rejected")
	}
	e.Rules = 0
	if candidateReady(doc, e) {
		t.Fatal("accepted API before rules initialization")
	}
}

func TestProviderHeadersAndRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Private") != "" {
			t.Error("credential leaked across origins")
		}
		fmt.Fprintln(w, "payload:\n  - example.com")
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Private") != "test" {
			t.Error("provider header missing")
		}
		http.Redirect(w, r, target.URL, 302)
	}))
	defer origin.Close()
	_, err := downloadHeaders(context.Background(), origin.URL, 1000, "", "", http.Header{"X-Private": []string{"test"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHistoryOnlyCommittedAndScoped(t *testing.T) {
	p, s, old, next, _ := txFixture(t)
	s.Subscriptions[0].Generation = old.Path
	if err := saveSettings(p, s); err != nil {
		t.Fatal(err)
	}
	if err := markCommitted(old.Path); err != nil {
		t.Fatal(err)
	}
	discardUncommitted(p, next.Path)
	if _, err := os.Stat(next.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed candidate retained")
	}
	pruneGenerations(p)
	if _, err := os.Stat(old.Path); err != nil {
		t.Fatal("active version pruned")
	}
}

func TestDigestRejectsMismatch(t *testing.T) {
	if verifyDigest([]byte("modified"), strings.Repeat("0", 64)) == nil {
		t.Fatal("accepted invalid checksum")
	}
}

func TestTunGenerationsPreserveSubscriptionRollback(t *testing.T) {
	p := testPaths(t)
	s := DefaultSettings()
	s.Active = "source"
	sub := Subscription{ID: "source", Name: "test"}
	first, err := buildGeneration(context.Background(), p, s, sub, []byte("proxies: [{name: first, type: socks5, server: example.com, port: 1080}]"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildGeneration(context.Background(), p, s, sub, []byte("proxies: [{name: second, type: socks5, server: example.com, port: 1080}]"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first.Path, second.Path} {
		if err = markCommitted(path); err != nil {
			t.Fatal(err)
		}
	}
	current := second
	secondContent := generationContent(second.Path)
	for _, enabled := range []bool{true, false, true, false} {
		s.Tun = enabled
		current, err = cloneGeneration(p, s, current.Path)
		if err != nil {
			t.Fatal(err)
		}
		if err = markCommitted(current.Path); err != nil {
			t.Fatal(err)
		}
	}
	sub.Generation = current.Path
	s.Subscriptions = []Subscription{sub}
	if err = saveSettings(p, s); err != nil {
		t.Fatal(err)
	}
	if err = switchLink(p.Current(), current.Path); err != nil {
		t.Fatal(err)
	}
	pruneGenerations(p)
	if _, err = os.Stat(first.Path); err != nil {
		t.Fatal("TUN toggles erased previous subscription", err)
	}
	if generationContent(current.Path) == generationContent(first.Path) || generationContent(current.Path) != secondContent {
		t.Fatal("incorrect content identity")
	}
}
