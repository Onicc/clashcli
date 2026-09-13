package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadProxyEnvironment(t *testing.T) {
	// net/http caches its environment once per process. Use real subprocesses
	// so each case tests production behavior without relying on test ordering.
	if os.Getenv("CLASHCLI_TEST_DOWNLOAD_PROXY") == "1" {
		client := downloadClient()
		defer client.CloseIdleConnections()
		req, err := http.NewRequest("GET", os.Getenv("CLASHCLI_TEST_PROXY_TARGET"), nil)
		if err != nil {
			t.Fatal(err)
		}
		proxy, err := client.Transport.(*http.Transport).Proxy(req)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if proxy != nil {
			got = proxy.String()
		}
		if got != os.Getenv("CLASHCLI_TEST_PROXY_EXPECT") {
			t.Fatalf("unexpected proxy selection: %q", got)
		}
		if os.Getenv("CLASHCLI_TEST_PROXY_FETCH") == "1" {
			d, err := download(context.Background(), req.URL.String(), 100, "", "")
			if err != nil || string(d.Body) != "via proxy" {
				t.Fatalf("download did not use proxy: %v", err)
			}
		}
		return
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() || r.URL.Host != "download.invalid" {
			t.Error("expected an HTTP forward-proxy request")
		}
		fmt.Fprint(w, "via proxy")
	}))
	defer proxy.Close()
	cases := []struct {
		name, target, want string
		env                []string
		fetch              bool
	}{
		{"lowercase HTTP", "http://download.invalid/file", proxy.URL, []string{"http_proxy=" + proxy.URL}, true},
		{"uppercase HTTP", "http://download.invalid/file", proxy.URL, []string{"HTTP_PROXY=" + proxy.URL}, true},
		{"lowercase HTTPS", "https://download.invalid/file", proxy.URL, []string{"https_proxy=" + proxy.URL}, false},
		{"uppercase takes precedence", "https://download.invalid/file", proxy.URL, []string{"HTTPS_PROXY=" + proxy.URL, "https_proxy=http://127.0.0.1:1"}, false},
		{"SOCKS via HTTPS variable", "https://download.invalid/file", "socks5://127.0.0.1:1080", []string{"https_proxy=socks5://127.0.0.1:1080"}, false},
		{"no proxy", "https://download.invalid/file", "", nil, false},
		{"no_proxy host", "https://download.invalid/file", "", []string{"https_proxy=" + proxy.URL, "no_proxy=download.invalid"}, false},
		{"NO_PROXY wildcard", "https://download.invalid/file", "", []string{"HTTPS_PROXY=" + proxy.URL, "NO_PROXY=*"}, false},
		{"NO_PROXY subnet", "http://10.1.2.3/file", "", []string{"HTTP_PROXY=" + proxy.URL, "NO_PROXY=10.0.0.0/8"}, false},
		{"localhost bypass", "http://localhost/file", "", []string{"http_proxy=" + proxy.URL}, false},
		{"IPv4 loopback bypass", "http://127.0.0.1/file", "", []string{"http_proxy=" + proxy.URL}, false},
		{"IPv6 loopback bypass", "http://[::1]/file", "", []string{"http_proxy=" + proxy.URL}, false},
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(exe, "-test.run=^TestDownloadProxyEnvironment$")
			for _, item := range os.Environ() {
				key, _, _ := strings.Cut(item, "=")
				switch strings.ToUpper(key) {
				case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "REQUEST_METHOD":
					continue
				}
				if !strings.HasPrefix(key, "CLASHCLI_TEST_PROXY_") && key != "CLASHCLI_TEST_DOWNLOAD_PROXY" {
					cmd.Env = append(cmd.Env, item)
				}
			}
			cmd.Env = append(cmd.Env, tc.env...)
			cmd.Env = append(cmd.Env, "CLASHCLI_TEST_DOWNLOAD_PROXY=1", "CLASHCLI_TEST_PROXY_TARGET="+tc.target, "CLASHCLI_TEST_PROXY_EXPECT="+tc.want)
			if tc.fetch {
				cmd.Env = append(cmd.Env, "CLASHCLI_TEST_PROXY_FETCH=1")
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("proxy subprocess: %v\n%s", err, out)
			}
		})
	}
}

func TestDownloadTimeoutAndAPIIsolation(t *testing.T) {
	client := downloadClient()
	defer client.CloseIdleConnections()
	if client.Timeout != 5*time.Minute {
		t.Fatalf("large downloads need a bounded five-minute timeout, got %s", client.Timeout)
	}
	apiClient := httpClient(30 * time.Second)
	defer apiClient.CloseIdleConnections()
	if apiClient.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("API must not use download proxy or expose its secret to a proxy")
	}
}

func TestDownloadIncompleteBody(t *testing.T) {
	for _, mode := range []string{"truncated", "timeout", "retry succeeds"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				w.Header().Set("Content-Length", "9")
				fmt.Fprint(w, "abc")
				if mode == "retry succeeds" && n == 3 {
					fmt.Fprint(w, "defghi")
				} else if mode == "timeout" {
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			client := downloadClient()
			client.Timeout = 200 * time.Millisecond
			d, err := downloadWithClient(context.Background(), server.URL+"/PRIVATE_TOKEN", 100, "", "", nil, client)
			if attempts.Load() != 3 {
				t.Fatalf("wanted three attempts, got %d: %v", attempts.Load(), err)
			}
			if mode == "retry succeeds" {
				if err != nil || string(d.Body) != "abcdefghi" {
					t.Fatalf("retry did not return a complete new body: %v", err)
				}
				return
			}
			if err == nil || len(d.Body) != 0 {
				t.Fatal("partial download must not be accepted")
			}
			for _, detail := range []string{"内容不完整", "3/9 字节", "127.0.0.1"} {
				if !strings.Contains(err.Error(), detail) {
					t.Fatalf("missing useful diagnostic %q: %v", detail, err)
				}
			}
			cause := "unexpected EOF"
			if mode == "timeout" {
				cause = "Client.Timeout"
			}
			if !strings.Contains(err.Error(), cause) || strings.Contains(err.Error(), "PRIVATE_TOKEN") {
				t.Fatalf("missing cause or leaked token: %v", err)
			}
		})
	}
}

func TestSudoPreservesOnlyDownloadAndDesktopEnvironment(t *testing.T) {
	got := sudoArguments("/usr/local/bin/clashcli", []string{"init", "--no-start"})
	want := []string{"--preserve-env=XDG_CURRENT_DESKTOP,http_proxy,https_proxy,no_proxy,HTTP_PROXY,HTTPS_PROXY,NO_PROXY", "--", "/usr/local/bin/clashcli", "init", "--no-start"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected sudo arguments: %v", got)
	}
}
