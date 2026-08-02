package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestGPTChromeProbe 真开一次 Chrome(端口模式 + 直连页面 WS),验证手搓 WebSocket + CDP 通路:
// 握手、Runtime.evaluate 往返、状态快照、navigator.webdriver 是否为假(反检测的关键)。
// 需 PROBE_GPT_CDP=1 才跑(会弹一个 Chrome 窗口),正常 go test 跳过。
func TestGPTChromeProbe(t *testing.T) {
	if os.Getenv("PROBE_GPT_CDP") != "1" {
		t.Skip("set PROBE_GPT_CDP=1 to run (opens a real Chrome window)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := newGPTDriver(ctx, "data:text/html,<title>probe</title><h1>hello</h1><input type=email>")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer d.Close()
	time.Sleep(1500 * time.Millisecond)

	st, err := d.state(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Logf("url=%q title=%q inputs=%d buttons=%d", st.URL, st.Title, len(st.Inputs), len(st.Buttons))
	if st.Title != "probe" {
		t.Errorf("title=%q want probe", st.Title)
	}

	// navigator.webdriver 应为 undefined/false(端口+页面WS不触发 AutomationControlled)。
	var wd any
	if err := d.chrome.Evaluate(ctx, `String(navigator.webdriver)`, &wd); err != nil {
		t.Fatalf("eval webdriver: %v", err)
	}
	t.Logf("navigator.webdriver = %v", wd)
	if s, _ := wd.(string); s == "true" {
		t.Errorf("navigator.webdriver=true, 仍会被判机器人")
	}

	// 输入原语往返。
	if err := d.insertTrusted(ctx, "input[type=email]", "probe@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var val string
	if err := d.chrome.Evaluate(ctx, `document.querySelector('input').value`, &val); err != nil {
		t.Fatalf("eval value: %v", err)
	}
	if val != "probe@example.com" {
		t.Errorf("input value=%q want probe@example.com", val)
	}
}
