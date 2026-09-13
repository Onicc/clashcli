package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const CoreVersion = "v1.19.30"
const UIVersion = "v1.273.1"
const DefaultCalendar = "*-*-* 00,12:00:00"
const maxSubscription = 16 << 20

func httpClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 10 * time.Second}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("重定向次数过多")
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("拒绝 HTTPS 降级重定向")
		}
		return nil
	}}
}

// Downloads may use the caller's explicit proxy. Keep the control API's client
// direct: its credentials and recovery path must never depend on that proxy.
func downloadClient() *http.Client {
	client := httpClient(5 * time.Minute)
	transport := client.Transport.(*http.Transport)
	transport.Proxy = http.ProxyFromEnvironment
	transport.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second
	return client
}

type Download struct {
	Body           []byte
	ETag, Modified string
	Unchanged      bool
}

func download(ctx context.Context, raw string, limit int64, etag, modified string) (Download, error) {
	return downloadHeaders(ctx, raw, limit, etag, modified, nil)
}
func downloadHeaders(ctx context.Context, raw string, limit int64, etag, modified string, headers http.Header) (Download, error) {
	return downloadWithClient(ctx, raw, limit, etag, modified, headers, downloadClient())
}

func downloadWithClient(ctx context.Context, raw string, limit int64, etag, modified string, headers http.Header, client *http.Client) (Download, error) {
	defer client.CloseIdleConnections()
	var result Download
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return result, errors.New("链接必须为不含用户名密码的 HTTP(S) URL")
	}
	redirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := redirect(req, via); err != nil {
			return err
		}
		if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
			for key := range headers {
				req.Header.Del(key)
			}
		}
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if e != nil {
			return result, errors.New("无效下载链接")
		}
		req.Header.Set("User-Agent", "clash.meta/"+strings.TrimPrefix(CoreVersion, "v")+" clashcli")
		for key, values := range headers {
			req.Header[key] = append([]string(nil), values...)
		}
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		if modified != "" {
			req.Header.Set("If-Modified-Since", modified)
		}
		resp, e := client.Do(req)
		if e != nil {
			err = fmt.Errorf("下载 %s 失败: %s", u.Host, Redact(e.Error()))
			continue
		}
		if resp.StatusCode == http.StatusNotModified {
			resp.Body.Close()
			return Download{Unchanged: true, ETag: etag, Modified: modified}, nil
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			err = fmt.Errorf("下载 %s 返回 HTTP %d", u.Host, resp.StatusCode)
			if resp.StatusCode >= 500 || resp.StatusCode == 429 {
				continue
			}
			return result, err
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if e != nil {
			progress := fmt.Sprintf("%d 字节", len(b))
			if resp.ContentLength >= 0 {
				progress = fmt.Sprintf("%d/%d 字节", len(b), resp.ContentLength)
			}
			err = fmt.Errorf("下载 %s 内容不完整（已收到 %s）: %s", u.Host, progress, Redact(e.Error()))
			continue
		}
		if int64(len(b)) > limit {
			return result, errors.New("下载内容超出大小限制")
		}
		if len(b) == 0 {
			return result, errors.New("下载内容为空")
		}
		return Download{Body: b, ETag: resp.Header.Get("ETag"), Modified: resp.Header.Get("Last-Modified")}, nil
	}
	return result, err
}

var coreHashes = map[string]string{"amd64": "cbe553d0319a414bd3a372c5976a252155b2c4882b66bce88a4d6bba9571a553", "arm64": "58896873736d28628f66de3677c8654fa0f180662523148e136cff4f6e890069"}

func verifyDigest(b []byte, digest string) error {
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != strings.TrimPrefix(digest, "sha256:") {
		return errors.New("SHA-256 校验失败")
	}
	return nil
}
func installCore(ctx context.Context, p Paths, local string) error {
	var b []byte
	if local != "" {
		var err error
		b, err = os.ReadFile(local)
		if err != nil {
			return err
		}
		if len(b) > 128<<20 {
			return errors.New("内核文件过大")
		}
	} else {
		hash, ok := coreHashes[runtime.GOARCH]
		if !ok {
			return errors.New("仅支持 amd64/arm64")
		}
		arch := runtime.GOARCH
		if arch == "amd64" {
			arch = "amd64-v1"
		}
		r, err := download(ctx, "https://github.com/MetaCubeX/mihomo/releases/download/"+CoreVersion+"/mihomo-linux-"+arch+"-"+CoreVersion+".gz", 64<<20, "", "")
		if err != nil {
			return err
		}
		if err = verifyDigest(r.Body, hash); err != nil {
			return err
		}
		b = r.Body
	}
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(strings.NewReader(string(b)))
		if err != nil {
			return err
		}
		b, err = io.ReadAll(io.LimitReader(zr, 128<<20+1))
		zr.Close()
		if err != nil {
			return err
		}
	}
	if len(b) < 4 || string(b[:4]) != "\x7fELF" || len(b) > 128<<20 {
		return errors.New("需要 Linux mihomo ELF 程序或 gzip 压缩包")
	}
	stage := p.Core() + ".candidate"
	if err := atomicWrite(stage, b, 0700); err != nil {
		return err
	}
	defer os.Remove(stage)
	out, err := commandOutput(ctx, stage, "-v")
	if err != nil || !strings.Contains(out, "Mihomo") {
		return errors.New("内核无法执行或不是 mihomo")
	}
	return os.Rename(stage, p.Core())
}
func installUI(ctx context.Context, p Paths, local string) error {
	var b []byte
	if local != "" {
		var err error
		b, err = os.ReadFile(local)
		if err != nil {
			return err
		}
	} else {
		r, err := download(ctx, "https://github.com/MetaCubeX/metacubexd/releases/download/"+UIVersion+"/compressed-dist.tgz", 32<<20, "", "")
		if err != nil {
			return err
		}
		if err = verifyDigest(r.Body, "a178e00b67acabcda2dcef00afa90be6a7bb261e466a67dad58c8478d9553603"); err != nil {
			return err
		}
		b = r.Body
	}
	base := filepath.Join(p.Data, "ui")
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(base, "version-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(stage)
		}
	}()
	if err = extractUI(b, stage); err != nil {
		return err
	}
	old, _ := os.Readlink(filepath.Join(base, "current"))
	if err = switchLink(filepath.Join(base, "current"), stage); err != nil {
		return err
	}
	keep = true
	if inside(base, old) && filepath.Dir(old) == base {
		_ = os.RemoveAll(old)
	}
	return nil
}
func extractUI(b []byte, dir string) error {
	if len(b) > 32<<20 {
		return errors.New("UI 压缩包过大")
	}
	zr, err := gzip.NewReader(strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var size int64
	count := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		count++
		size += h.Size
		if count > 10000 || size > 128<<20 || h.Size < 0 {
			return errors.New("UI 解压大小超限")
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" || name == "." {
			continue
		}
		dest := filepath.Join(dir, name)
		if filepath.IsAbs(name) || !inside(dir, dest) || strings.Contains(name, "\\") {
			return errors.New("UI 包包含不安全路径")
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(dest, 0700)
		case tar.TypeReg, tar.TypeRegA:
			if err = os.MkdirAll(filepath.Dir(dest), 0700); err == nil {
				var data []byte
				data, err = io.ReadAll(io.LimitReader(tr, h.Size+1))
				if err == nil {
					err = atomicWrite(dest, data, 0600)
				}
			}
		default:
			return errors.New("UI 包不允许链接或设备文件")
		}
		if err != nil {
			return err
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return errors.New("UI 包根目录缺少 index.html")
	}
	return nil
}
func ensureGeo(ctx context.Context, p Paths) error {
	files := map[string]string{"geoip.dat": "geoip.dat", "geosite.dat": "GeoSite.dat", "geoip.metadb": "geoip.metadb"}
	missing := false
	for _, dest := range files {
		if _, err := os.Stat(filepath.Join(p.Data, dest)); err != nil {
			missing = true
		}
	}
	if !missing {
		return nil
	}
	r, err := download(ctx, "https://api.github.com/repos/MetaCubeX/meta-rules-dat/releases/latest", 2<<20, "", "")
	if err != nil {
		return err
	}
	var release struct {
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err = json.Unmarshal(r.Body, &release); err != nil {
		return err
	}
	for name, dest := range files {
		if _, err := os.Stat(filepath.Join(p.Data, dest)); err == nil {
			continue
		}
		found := false
		for _, a := range release.Assets {
			if a.Name != name {
				continue
			}
			found = true
			if !strings.HasPrefix(a.Digest, "sha256:") {
				return errors.New("Geo 数据缺少官方校验值")
			}
			d, err := download(ctx, a.URL, 64<<20, "", "")
			if err != nil {
				return err
			}
			if err = verifyDigest(d.Body, a.Digest); err != nil {
				return err
			}
			if err = atomicWrite(filepath.Join(p.Data, dest), d.Body, 0600); err != nil {
				return err
			}
		}
		if !found {
			return fmt.Errorf("Geo 发行包缺少 %s", name)
		}
	}
	return nil
}
