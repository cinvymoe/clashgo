package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer 起一个假的控制器，记录收到的请求。
func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "s3cret"), &seen
}

// TestVersionSendsAuth 请求必须带上 Bearer 密钥。
func TestVersionSendsAuth(t *testing.T) {
	var gotAuth string
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"version":"v1.19.33","meta":true}`))
	})
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q，期望 %q", gotAuth, "Bearer s3cret")
	}
	if v["version"] != "v1.19.33" {
		t.Errorf("version = %v", v["version"])
	}
}

// TestUnauthorized 401 要给出可操作的错误信息，而不是裸状态码。
func TestUnauthorized(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := c.Version(context.Background())
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if !strings.Contains(err.Error(), "密钥") {
		t.Errorf("错误信息应提示检查密钥，实际: %v", err)
	}
}

// TestErrorBodyIncluded 非 2xx 时要把内核返回的正文带进错误。
func TestErrorBodyIncluded(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`path is not allowed`))
	})
	err := c.ReloadConfig(context.Background(), "/tmp/x.yaml")
	if err == nil || !strings.Contains(err.Error(), "path is not allowed") {
		t.Fatalf("错误应包含内核返回的正文，实际: %v", err)
	}
}

// TestSelectEscapesGroupName 策略组名里可能有空格或中文，必须转义。
func TestSelectEscapesGroupName(t *testing.T) {
	var seenPath string
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Select(context.Background(), "节点 选择", "香港 01"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !strings.HasPrefix(seenPath, "/proxies/") {
		t.Errorf("请求路径异常: %q", seenPath)
	}
	if strings.Contains(seenPath, " ") {
		t.Errorf("路径未转义: %q", seenPath)
	}
}

// TestProxiesParsed /proxies 的响应要能解出策略组与当前选择。
func TestProxiesParsed(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"proxies":{
			"GLOBAL":{"type":"Selector","name":"GLOBAL","now":"DIRECT","all":["DIRECT","node-a"],"alive":true},
			"node-a":{"type":"Shadowsocks","name":"node-a","alive":true,
			  "history":[{"time":"2026-01-01T00:00:00Z","delay":42}]}
		}}`))
	})
	got, err := c.Proxies(context.Background())
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应解析出 2 项，实际 %d", len(got))
	}
	if g := got["GLOBAL"]; g.Now != "DIRECT" || len(g.All) != 2 {
		t.Errorf("GLOBAL 解析错误: %+v", g)
	}
	if n := got["node-a"]; n.History[0].Delay != 42 {
		t.Errorf("history 延迟解析错误: %+v", n.History)
	}
}

// TestNormalizeBaseURL 各种监听地址都要能转成可访问的 URL。
func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:9090":         "http://127.0.0.1:9090",
		"127.0.0.1:9090":       "http://127.0.0.1:9090",
		"[::1]:9090":           "http://[::1]:9090",
		"0.0.0.0":              "http://127.0.0.1:9090",
		"http://1.2.3.4:9090":  "http://1.2.3.4:9090",
		"https://1.2.3.4:9090": "https://1.2.3.4:9090",
		"":                     "",
	}
	for in, want := range cases {
		if got := normalizeBaseURL(in); got != want {
			t.Errorf("normalizeBaseURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestEmptyBaseURL 没有 external-controller 时要报明确错误，而不是构造出非法 URL。
func TestEmptyBaseURL(t *testing.T) {
	c := New("", "")
	_, err := c.Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "external-controller") {
		t.Fatalf("应提示缺少 external-controller，实际: %v", err)
	}
}

// TestUpgradeChannel 带 channel 时走 query 参数。
func TestUpgradeChannel(t *testing.T) {
	var gotURL string
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Upgrade(context.Background(), "alpha"); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if !strings.Contains(gotURL, "channel=alpha") {
		t.Errorf("URL 应带 channel 参数，实际 %q", gotURL)
	}
}
