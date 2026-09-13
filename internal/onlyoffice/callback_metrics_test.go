package onlyoffice

import (
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// callbackCount 读取 docflow_onlyoffice_callbacks_total 指定标签系列的当前值
// （计数器为进程级全局，测试一律取前后差值断言）。
func callbackCount(t *testing.T, status, result string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "docflow_onlyoffice_callbacks_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if got["status"] == status && got["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// 标签归一纯函数：2/4 保留，其余（1/3/6/7 与 0）归 other。
func TestCallbackStatusAndResultLabels(t *testing.T) {
	cases := []struct {
		status int64
		want   string
	}{
		{2, "2"}, {4, "4"}, {1, "other"}, {3, "other"}, {6, "other"}, {7, "other"}, {0, "other"},
	}
	for _, tc := range cases {
		if got := CallbackStatusLabel(tc.status); got != tc.want {
			t.Errorf("CallbackStatusLabel(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
	if got := CallbackResultLabel(nil); got != "0" {
		t.Errorf("CallbackResultLabel(nil) = %q, want 0", got)
	}
	if got := CallbackResultLabel(ErrInvalidToken); got != "1" {
		t.Errorf("CallbackResultLabel(err) = %q, want 1", got)
	}
}

// HandleCallback 全路径计数：保存成功 {2,0}、清理 {4,0}、验签失败 {2,1}、
// 强制保存 status=6 归 other、非 JSON body 归 other 且失败。
func TestHandleCallbackCountsMetric(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	fetcher := &fakeFetch{content: []byte("edited v2")}
	s := newTestService(store, newMemStorage(), fetcher, &fakeRecorder{}, nil)
	key := documentKey(file.ID, version.ID)

	assertDelta := func(name, status, result string, before float64, want float64) {
		t.Helper()
		if got := callbackCount(t, status, result) - before; got != want {
			t.Errorf("%s: docflow_onlyoffice_callbacks_total{status=%q,result=%q} 差值 = %v, want %v", name, status, result, got, want)
		}
	}

	// status=2 保存成功。
	before := callbackCount(t, "2", "0")
	body := callbackBody(t, testJWTSecret, map[string]any{"key": key, "status": 2, "url": sameOriginURL}, true)
	if err := s.HandleCallback(body, "", "", ""); err != nil {
		t.Fatalf("save callback: %v", err)
	}
	assertDelta("保存成功", "2", "0", before, 1)

	// 同 key 重复回调（幂等命中，仍计数——DS 确实又回调了一次）。
	before = callbackCount(t, "2", "0")
	if err := s.HandleCallback(body, "", "", ""); err != nil {
		t.Fatalf("duplicate callback: %v", err)
	}
	assertDelta("幂等重试", "2", "0", before, 1)

	// 验签失败（错密钥签名）：body 声称 status=2，result=1。
	before = callbackCount(t, "2", "1")
	badBody := callbackBody(t, "wrong-secret-wrong-secret-wrong!!", map[string]any{"key": key, "status": 2, "url": sameOriginURL}, true)
	if err := s.HandleCallback(badBody, "", "", ""); err == nil {
		t.Fatal("wrong-key callback should fail")
	}
	assertDelta("验签失败", "2", "1", before, 1)

	// status=4 清理成功。
	before = callbackCount(t, "4", "0")
	cleanup := callbackBody(t, testJWTSecret, map[string]any{"key": key, "status": 4}, true)
	if err := s.HandleCallback(cleanup, "", "", ""); err != nil {
		t.Fatalf("cleanup callback: %v", err)
	}
	assertDelta("清理成功", "4", "0", before, 1)

	// status=6（强制保存）成功保存，但归一为 other。
	fetcher.content = []byte("forcesave v3")
	before = callbackCount(t, "other", "0")
	force := callbackBody(t, testJWTSecret, map[string]any{"key": key, "status": 6, "url": sameOriginURL + "?t=2"}, true)
	if err := s.HandleCallback(force, "", "", ""); err != nil {
		t.Fatalf("forcesave callback: %v", err)
	}
	assertDelta("强制保存归 other", "other", "0", before, 1)

	// 非 JSON body：status 未知归 other，result=1。
	before = callbackCount(t, "other", "1")
	if err := s.HandleCallback([]byte("not json"), "", "", ""); err == nil {
		t.Fatal("bad body should fail")
	}
	assertDelta("坏 body", "other", "1", before, 1)
}
