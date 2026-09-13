package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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
	for _, file := range []string{p.Core(), filepath.Join(p.Data, "ui/current/index.html"), filepath.Join(p.Data, "geoip.dat"), filepath.Join(p.Data, "GeoSite.dat"), filepath.Join(p.Data, "geoip.metadb")} {
		info, err := os.Stat(file)
		if err != nil || info.Size() == 0 {
			t.Fatalf("missing or empty installed component: %s: %v", filepath.Base(file), err)
		}
	}
	s := DefaultSettings()
	sub := Subscription{ID: "live-smoke", Name: "live-smoke"}
	// This is a local fixture, not a private subscription or a working node.
	raw := []byte("proxies: [{name: fixture, type: socks5, server: 127.0.0.1, port: 9}]\nproxy-groups: [{name: PROXY, type: select, proxies: [fixture]}]\nrules: ['MATCH,DIRECT']\n")
	g, err := buildGeneration(ctx, p, s, sub, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = (RealCore{P: p, S: s}).Validate(ctx, &g); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS real candidate core startup and private Unix API validation; no system proxy, TUN or services changed")
}
