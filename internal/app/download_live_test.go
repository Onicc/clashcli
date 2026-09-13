package app

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Opt-in, unprivileged installation smoke test for a real Linux network. It
// uses the production installers and candidate core without touching systemd,
// the installed CLI/configuration, proxy settings, or TUN. Every file is private
// and temporary; RealCore.Validate always terminates its candidate process.
func TestLiveComponentInstall(t *testing.T) {
	if os.Getenv("CLASHCLI_TEST_LIVE_DOWNLOAD") != "1" {
		t.Skip("set CLASHCLI_TEST_LIVE_DOWNLOAD=1 on Linux to download real components")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("live component execution requires Linux")
	}
	var subscriptionURL string
	if os.Getenv("CLASHCLI_TEST_LIVE_SUB_STDIN") == "1" {
		fmt.Println("Ready for private subscription URL on stdin (not echoed by this test)")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			t.Fatal("could not read private subscription URL from stdin")
		}
		subscriptionURL = strings.TrimSpace(line)
		if err = validateSubInput("live-smoke", subscriptionURL); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the Unix socket path below the Linux limit, even with a long test name.
	dir, err := os.MkdirTemp("", "cc-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("cleaning private test directory: %v", err)
		}
	})
	p := NewPaths(dir)
	if err = p.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"mihomo download, SHA-256 and executable validation", func() error { return installCore(ctx, p, "") }},
		{"MetaCubeXD download, SHA-256 and safe extraction", func() error { return installUI(ctx, p, "") }},
		{"official Geo metadata, downloads and SHA-256", func() error { return ensureGeo(ctx, p) }},
	} {
		start := time.Now()
		if err = step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		t.Logf("PASS %s (%s)", step.name, time.Since(start).Round(time.Millisecond))
	}
	files := []string{p.Core(), filepath.Join(p.Data, "ui/current/index.html")}
	for _, name := range geoFiles {
		files = append(files, filepath.Join(p.Data, name))
	}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || info.Size() == 0 {
			t.Fatalf("missing or empty installed component: %s: %v", filepath.Base(file), err)
		}
	}
	s := DefaultSettings()
	if subscriptionURL != "" {
		// Reserve two distinct loopback ports; never claim the user's normal ports.
		one, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer one.Close()
		two, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		s.MixedPort = one.Addr().(*net.TCPAddr).Port
		s.ControllerPort = two.Addr().(*net.TCPAddr).Port
		one.Close()
		two.Close()
	}
	sub := Subscription{ID: "live-smoke", Name: "live-smoke"}
	// This is a local fixture, not a private subscription or a working node.
	raw := []byte("proxies: [{name: fixture, type: socks5, server: 127.0.0.1, port: 9}]\nproxy-groups: [{name: PROXY, type: select, proxies: [fixture]}]\nrules: ['IP-ASN,13335,DIRECT,no-resolve', 'MATCH,DIRECT']\n")
	if subscriptionURL != "" {
		d, err := download(ctx, subscriptionURL, maxSubscription, "", "")
		if err != nil {
			t.Fatal(err)
		}
		raw = d.Body
		t.Logf("Downloaded private subscription (%d bytes; contents not logged)", len(raw))
	}
	start := time.Now()
	g, err := buildGeneration(ctx, p, s, sub, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PASS subscription and provider snapshot preparation (%s)", time.Since(start).Round(time.Millisecond))
	if subscriptionURL != "" {
		liveDisableDNSListener(t, g)
	}
	start = time.Now()
	if err = (RealCore{P: p, S: s}).Validate(ctx, &g); err != nil {
		t.Fatalf("candidate validation after %s: %v", time.Since(start).Round(time.Millisecond), err)
	}
	t.Logf("PASS real candidate core startup and private Unix API validation (%s)", time.Since(start).Round(time.Millisecond))
	if subscriptionURL != "" {
		liveSubscriptionRuntime(t, ctx, p, s, sub, g)
	}
	t.Log("PASS no system proxy, TUN or services changed; temporary processes and files cleaned on return")
}

func liveDisableDNSListener(t *testing.T, g Generation) Document {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(g.Path, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err = yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	// The real runtime test uses an explicit HTTP proxy, not host DNS or routing.
	doc["dns"].(map[string]any)["listen"] = ""
	b, err = yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = atomicWrite(filepath.Join(g.Path, "config.yaml"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return doc
}

func liveSubscriptionRuntime(t *testing.T, ctx context.Context, p Paths, s Settings, sub Subscription, g Generation) {
	t.Helper()
	doc := liveDisableDNSListener(t, g)
	nodes, _ := doc["proxies"].([]any)
	if len(nodes) == 0 {
		t.Fatal("private runtime smoke test currently requires an inline node")
	}
	node := nodes[0].(map[string]any)["name"].(string)
	s.Active = sub.ID
	sub.Generation = g.Path
	s.Subscriptions = []Subscription{sub}
	if err := saveSettings(p, s); err != nil {
		t.Fatal(err)
	}
	if err := markCommitted(g.Path); err != nil {
		t.Fatal(err)
	}
	if err := switchLink(p.Current(), g.Path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	child := exec.CommandContext(ctx, p.Core(), "-d", p.Data, "-f", filepath.Join(p.Current(), "config.yaml"))
	child.Env = coreEnv(p)
	var output cappedBuffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }()
	defer func() {
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = child.Process.Kill()
			<-done
		}
	}()
	core := RealCore{P: p, S: s}
	if err := core.Check(ctx, g); err != nil {
		t.Fatalf("private runtime health: %v; %s", err, safeCoreOutput(output.String()))
	}
	api := APIFor(s)
	if err := api.Do(ctx, "PUT", "/proxies/GLOBAL", map[string]string{"name": node}, nil); err != nil {
		t.Fatal(err)
	}
	if err := api.Do(ctx, "PATCH", "/configs", map[string]string{"mode": "global"}, nil); err != nil {
		t.Fatal(err)
	}
	client := httpClient(25 * time.Second)
	defer client.CloseIdleConnections()
	proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", s.MixedPort))
	client.Transport.(*http.Transport).Proxy = http.ProxyURL(proxy)
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.gstatic.com/generate_204", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTPS request through private node: %s", Redact(err.Error()))
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("private node returned HTTP %d, expected 204", resp.StatusCode)
	}
	t.Log("PASS HTTPS 204 through an actual subscription node using only an explicit temporary loopback proxy")
	next, err := cloneGeneration(p, s, g.Path)
	if err != nil {
		t.Fatal(err)
	}
	liveDisableDNSListener(t, next)
	if err = core.Validate(ctx, &next); err != nil {
		t.Fatal(err)
	}
	s.Subscriptions[0].Generation = next.Path
	if err = (Manager{P: p, Core: core}).Apply(ctx, next, s, s.Revision); err != nil {
		t.Fatal(err)
	}
	current, err := os.Readlink(p.Current())
	if err != nil || current != next.Path {
		t.Fatal("atomic generation switch did not commit", err)
	}
	select {
	case <-done:
		t.Fatal("core exited during reload")
	default:
	}
	if err = core.Check(ctx, next); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS private subscription atomic generation replacement, API reload and health check without restarting the core")
}
