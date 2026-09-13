package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGeoUpgradeAddsVerifiedASNAndPreservesExistingData(t *testing.T) {
	for _, mode := range []string{"valid", "bad digest", "missing asset"} {
		t.Run(mode, func(t *testing.T) {
			p := testPaths(t)
			for _, name := range []string{"geoip.dat", "GeoSite.dat", "geoip.metadb"} {
				if err := atomicWrite(filepath.Join(p.Data, name), []byte("existing "+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			asn := []byte("fixture ASN database")
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(asn))
			if mode == "bad digest" {
				digest = "sha256:" + strings.Repeat("0", 64)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/asn" {
					_, _ = w.Write(asn)
					return
				}
				assets := []map[string]string{}
				if mode != "missing asset" {
					assets = append(assets, map[string]string{"name": "GeoLite2-ASN.mmdb", "digest": digest, "browser_download_url": "http://" + r.Host + "/asn"})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"assets": assets})
			}))
			defer server.Close()
			err := ensureGeoFrom(context.Background(), p, server.URL)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				b, err := os.ReadFile(filepath.Join(p.Data, "ASN.mmdb"))
				if err != nil || string(b) != string(asn) {
					t.Fatalf("ASN data not installed: %v", err)
				}
				info, err := os.Stat(filepath.Join(p.Data, "ASN.mmdb"))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatal("ASN data must be private")
				}
				if err = ensureGeoFrom(context.Background(), p, server.URL); err != nil || requests.Load() != 2 {
					t.Fatal("complete installation must not download again", err, requests.Load())
				}
			} else {
				if err == nil {
					t.Fatal("accepted unverified or missing ASN data")
				}
				if _, err = os.Stat(filepath.Join(p.Data, "ASN.mmdb")); !os.IsNotExist(err) {
					t.Fatal("failed download left an installed ASN database")
				}
			}
			for _, name := range []string{"geoip.dat", "GeoSite.dat", "geoip.metadb"} {
				b, err := os.ReadFile(filepath.Join(p.Data, name))
				if err != nil || string(b) != "existing "+name {
					t.Fatal("existing data was modified", name, err)
				}
			}
		})
	}
}

func TestOfflineGeoImportIncludesASN(t *testing.T) {
	p := testPaths(t)
	dir := t.TempDir()
	for _, name := range []string{"geoip.dat", "GeoSite.dat", "geoip.metadb", "ASN.mmdb"} {
		if err := atomicWrite(filepath.Join(dir, name), []byte("offline "+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := importGeo(p, dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"geoip.dat", "GeoSite.dat", "geoip.metadb", "ASN.mmdb"} {
		b, err := os.ReadFile(filepath.Join(p.Data, name))
		if err != nil || string(b) != "offline "+name {
			t.Fatal("offline asset not imported", name, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "ASN.mmdb")); err != nil {
		t.Fatal(err)
	}
	if err := importGeo(p, dir); err == nil || !strings.Contains(err.Error(), "ASN.mmdb") {
		t.Fatal("missing offline ASN asset must be identified", err)
	}
}

func TestCoreDiagnosticsKeepInitializationProgressAndErrors(t *testing.T) {
	for _, log := range []string{
		`level=info msg="Can't find ASN.mmdb, start download"`,
		`ERROR: parse configuration failed`,
		`FATAL: initialization stopped`,
		`panic: runtime failure`,
	} {
		if got := safeCoreOutput(log); got != log {
			t.Fatalf("lost useful diagnostics: %q", got)
		}
	}
	if got := safeCoreOutput("  \n\n"); got != "请检查内核兼容性与配置依赖" {
		t.Fatal("empty output has no useful fallback", got)
	}
	got := safeCoreOutput("info: first\ninfo: second\ninfo: third\ninfo: fourth\ninfo: fifth\ninfo: sixth https://example.com/PRIVATE?token=PRIVATE")
	if strings.Contains(got, "first") || strings.Contains(got, "PRIVATE") || !strings.Contains(got, "sixth") {
		t.Fatal("fallback must be bounded and redacted", got)
	}
	got = safeCoreOutput("info: unneeded\nERROR: password=PRIVATE token=PRIVATE uuid=PRIVATE")
	if strings.Contains(got, "PRIVATE") || strings.Contains(got, "unneeded") {
		t.Fatal("error output must be prioritized and redacted", got)
	}
	for _, log := range []string{
		`INFO Authorization: Bearer PRIVATE`,
		`INFO Proxy-Authorization: Basic PRIVATE`,
		`INFO password="two PRIVATE words"`,
		`INFO secret='two PRIVATE words'`,
		`INFO {"authorization": "Bearer PRIVATE"}`,
	} {
		if got := safeCoreOutput(log); strings.Contains(got, "PRIVATE") {
			t.Fatal("initialization fallback leaked a credential", got)
		}
	}
}

func TestCandidateStagesASNAndReportsProcessFailure(t *testing.T) {
	p := testPaths(t)
	for _, name := range []string{"geoip.dat", "GeoSite.dat", "geoip.metadb", "ASN.mmdb"} {
		if err := atomicWrite(filepath.Join(p.Data, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = -d ]; then shift; probe="$1"; fi
  shift
done
for asset in geoip.dat GeoSite.dat geoip.metadb ASN.mmdb; do
  if [ ! -L "$probe/$asset" ] || [ ! -r "$probe/$asset" ]; then
    echo 'ERROR missing staged Geo dependency'
    exit 42
  fi
done
echo 'FATAL deliberate fixture rejection'
exit 17
`
	if err := atomicWrite(p.Core(), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s := DefaultSettings()
	g, err := buildGeneration(context.Background(), p, s, Subscription{ID: "fixture"}, []byte("proxies: [{name: fixture, type: socks5, server: 127.0.0.1, port: 9}]\nrules: ['MATCH,DIRECT']\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = (RealCore{P: p, S: s}).Validate(context.Background(), &g)
	if err == nil || !strings.Contains(err.Error(), "exit status 17") || !strings.Contains(err.Error(), "FATAL deliberate") {
		t.Fatal("lost process exit status, fatal output, or staged ASN link", err)
	}
	if err = atomicWrite(p.Core(), []byte("#!/bin/sh\necho 'INFO fixture initialization stalled'\nexec sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = (RealCore{P: p, S: s}).Validate(ctx, &g)
	if err == nil || !strings.Contains(err.Error(), "预检超时或已取消") || !strings.Contains(err.Error(), "initialization stalled") {
		t.Fatal("lost timeout cause or last initialization progress", err)
	}
	entries, err := os.ReadDir(p.Run)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed/timed-out candidate left its private directory", err)
	}
}
