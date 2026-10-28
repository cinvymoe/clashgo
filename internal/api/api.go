// Package api 是 mihomo 外部控制器（REST）的最小客户端。
//
// 只包装 clashgo 真正用到的接口，避免引入一个庞大的 SDK。
// 所有请求一律直连（不使用 http_proxy），否则「开启系统代理」之后
// 访问 127.0.0.1:9090 会绕一圈代理反而失败。
package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 指向一个 mihomo 外部控制器。
type Client struct {
	BaseURL string // 形如 http://127.0.0.1:9090
	Secret  string
	HTTP    *http.Client
}

// New 构造客户端。addr 可以是 "0.0.0.0:9090"、"127.0.0.1:9090" 或带 scheme 的 URL。
func New(addr, secret string) *Client {
	return &Client{
		BaseURL: normalizeBaseURL(addr),
		Secret:  secret,
		HTTP: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				Proxy:           nil, // 必须直连
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// normalizeBaseURL 把内核配置里的监听地址转成客户端可用的 URL。
// 监听在 0.0.0.0 / :: / 空 host 时，客户端要访问回环地址。
func normalizeBaseURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	scheme := "http://"
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		i := strings.Index(addr, "://")
		scheme, addr = addr[:i+3], addr[i+3:]
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// 没写端口（或 IPv6 没加方括号），按 mihomo 默认 9090
		port = "9090"
		host = strings.Trim(addr, "[]")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return scheme + net.JoinHostPort(host, port)
}

// Version 返回内核版本信息。
func (c *Client) Version(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, http.MethodGet, "/version", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Proxies 返回全部代理与策略组，key 为名字。
func (c *Client) Proxies(ctx context.Context) (map[string]Proxy, error) {
	var payload struct {
		Proxies map[string]Proxy `json:"proxies"`
	}
	if err := c.do(ctx, http.MethodGet, "/proxies", nil, &payload); err != nil {
		return nil, err
	}
	return payload.Proxies, nil
}

// Proxy 是 /proxies 里的一项。
type Proxy struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Now     string   `json:"now,omitempty"`
	All     []string `json:"all,omitempty"`
	History []Delay  `json:"history,omitempty"`
	Alive   bool     `json:"alive"`
}

// Delay 是一次延迟测试结果。
type Delay struct {
	Time  time.Time `json:"time"`
	Delay int       `json:"delay"`
}

// Select 切换策略组当前节点。
func (c *Client) Select(ctx context.Context, group, name string) error {
	body := map[string]string{"name": name}
	return c.do(ctx, http.MethodPut, "/proxies/"+urlPathEscape(group), body, nil)
}

// ReloadConfig 让内核重新读取指定配置文件（进程内重载，等价于 SIGHUP）。
// 注意：路径必须在内核允许的目录内，否则内核返回 400。
func (c *Client) ReloadConfig(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodPut, "/configs", map[string]string{"path": path}, nil)
}

// Upgrade 触发内核自升级。
func (c *Client) Upgrade(ctx context.Context, channel string) error {
	p := "/upgrade"
	if channel != "" {
		p += "?channel=" + urlQueryEscape(channel)
	}
	return c.do(ctx, http.MethodPost, p, nil, nil)
}

// Restart 请求内核重启自身（由内核 exec 自己，不是 systemd 重启）。
func (c *Client) Restart(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/restart", nil, nil)
}

// do 是统一请求入口：拼 Authorization、处理非 2xx、把响应解到 out。
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if c.BaseURL == "" {
		return fmt.Errorf("未配置 external-controller，无法访问内核 API")
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求体失败: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求内核 API 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("内核 API 认证失败，请检查密钥（HTTP 401）")
		}
		if msg := strings.TrimSpace(string(detail)); msg != "" {
			return fmt.Errorf("内核 API 返回 %d: %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("内核 API 返回 %d", resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("解析内核响应失败: %w", err)
	}
	return nil
}

// urlPathEscape / urlQueryEscape 单独抽出来，避免调用方忘了转义策略组名。
func urlPathEscape(s string) string { return url.PathEscape(s) }

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
