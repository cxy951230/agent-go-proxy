package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// 临时探针:打开真实登录页,测「DOM 判定就绪」要多久,并确认状态机在微软真实页面上
// 能识别成 email。只读页面,不输入、不提交。默认跳过,需要 PROBE_OUTLOOK=1 才跑。
func TestProbeOutlookReady(t *testing.T) {
	if os.Getenv("PROBE_OUTLOOK") == "" {
		t.Skip("设置 PROBE_OUTLOOK=1 才跑这个探针")
	}
	start := time.Now()
	session, err := launchChrome(outlookLoginStartURL)
	if err != nil {
		t.Fatalf("launchChrome: %v", err)
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	target, err := waitForPageTarget(ctx, session, 15*time.Second)
	if err != nil {
		t.Fatalf("waitForPageTarget: %v", err)
	}
	t.Logf("标签页就绪耗时 %v (url=%s)", time.Since(start).Round(time.Millisecond), target.URL)

	page, err := session.AttachPage(ctx, target.TargetID)
	if err != nil {
		t.Fatalf("AttachPage: %v", err)
	}
	driver := &outlookAutoLogin{page: page}

	snap, state := driver.waitReady(ctx, outlookAutoReadyTimeout)
	t.Logf("DOM 稳定耗时 %v", time.Since(start).Round(time.Millisecond))
	t.Logf("识别状态=%q ready=%q title=%q inputs=%d buttons=%d",
		state, snap.Ready, snap.Title, len(snap.Inputs), len(snap.Buttons))
	for _, input := range snap.Inputs {
		t.Logf("  input type=%q name=%q aria=%q", input.Type, input.Name, input.Aria)
	}
	for _, button := range snap.Buttons {
		t.Logf("  button text=%q", button.Text)
	}
	if state != "email" {
		t.Errorf("真实登录页应识别成 email，实际是 %q", state)
	}
}
