package downloadfile

import (
	"AfdianToMarkdown/afdian"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func resolveHTTP(base, raw string) (*url.URL, error) {
	b, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("无效的来源地址")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("无效的资源地址")
	}
	u = b.ResolveReference(u)
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("资源地址必须为 HTTP(S)")
	}
	return u, nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// downloadFile streams to a temporary file; neither cookies nor signed URLs are logged.
func downloadFile(ctx context.Context, raw, destination, origin, referer, cookie string, expected int64) error {
	u, err := resolveHTTP(referer, raw)
	if err != nil {
		return err
	}
	base, err := resolveHTTP(origin, origin)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 30 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("下载重定向次数过多")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("不支持的重定向协议")
			}
			// Never forward the account cookie to CDN/object-storage hosts or subdomains.
			req.Header.Del("Cookie")
			if sameOrigin(req.URL, base) {
				req.Header.Set("Cookie", cookie)
			}
			return nil
		},
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		retry, err := downloadAttempt(ctx, client, u, base, destination, referer, cookie, expected)
		if err == nil {
			return nil
		}
		last = err
		if !retry {
			break
		}
		if attempt < 2 {
			if err := pause(ctx, time.Second*time.Duration(1<<attempt)); err != nil {
				return err
			}
		}
	}
	return last
}

func downloadAttempt(ctx context.Context, client *http.Client, u, origin *url.URL, destination, referer, cookie string, expected int64) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, fmt.Errorf("无法创建下载请求")
	}
	req.Header.Set("User-Agent", afdian.ChromeUserAgent)
	req.Header.Set("Referer", referer)
	if sameOrigin(u, origin) {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// url.Error includes the signed URL. Only expose the underlying cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return true, fmt.Errorf("下载请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode == 429 || resp.StatusCode >= 500, fmt.Errorf("下载返回 HTTP %d", resp.StatusCode)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	ext := strings.ToLower(filepath.Ext(destination))
	if (strings.Contains(contentType, "text/html") && ext != ".html" && ext != ".htm") ||
		(strings.Contains(contentType, "application/json") && ext != ".json") {
		return false, fmt.Errorf("下载返回了网页/JSON，可能是登录页面或失效链接")
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".download-*")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return true, fmt.Errorf("文件传输中断: %v", copyErr)
	}
	if closeErr != nil {
		return false, closeErr
	}
	if expected > 0 && n != expected {
		return true, fmt.Errorf("文件大小不符: 期望 %d，实际 %d", expected, n)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return true, fmt.Errorf("响应长度不符")
	}
	if n == 0 {
		return false, fmt.Errorf("下载返回空文件")
	}
	if err := os.Rename(tmp, destination); err != nil {
		return false, err
	}
	return false, nil
}
