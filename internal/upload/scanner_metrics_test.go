package upload

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// counterValue 从默认 registry 读取指定指标中标签完全匹配的系列当前值
// （计数器为进程级全局，测试一律取前后差值断言）。
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			match := true
			for k, v := range labels {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

type stubScanner struct{ err error }

func (s stubScanner) Scan(io.Reader) error { return s.err }

// CountingScanner 分类：nil -> clean；ErrMalwareDetected 包装 -> infected；
// 其余错误 -> error；且不改变内层错误语义。
func TestCountingScannerScanResults(t *testing.T) {
	infectedErr := fmt.Errorf("%w: EICAR-Test-File", ErrMalwareDetected)
	cases := []struct {
		name     string
		inner    Scanner
		wantErr  error
		wantKind string
	}{
		{"clean", stubScanner{nil}, nil, "clean"},
		{"infected", stubScanner{infectedErr}, infectedErr, "infected"},
		{"error", stubScanner{io.ErrUnexpectedEOF}, io.ErrUnexpectedEOF, "error"},
		{"reject scanner fail closed", RejectScanner{}, io.ErrUnexpectedEOF, "error"},
		{"nil 回退 allow", nil, nil, "clean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := counterValue(t, "docflow_scan_results_total", map[string]string{"result": tc.wantKind})
			scanner := NewCountingScanner(tc.inner)
			err := scanner.Scan(strings.NewReader("payload"))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Scan err = %v, want %v", err, tc.wantErr)
			}
			after := counterValue(t, "docflow_scan_results_total", map[string]string{"result": tc.wantKind})
			if after-before != 1 {
				t.Fatalf("docflow_scan_results_total{result=%q} 差值 = %v, want 1", tc.wantKind, after-before)
			}
		})
	}
}

// Start 创建会话计入 docflow_upload_sessions_total{status=created|failed}。
func TestStartCountsSessionMetric(t *testing.T) {
	store := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, storage, time.Hour, 1<<20, false, nil, nil)

	createdBefore := counterValue(t, "docflow_upload_sessions_total", map[string]string{"status": "created"})
	failedBefore := counterValue(t, "docflow_upload_sessions_total", map[string]string{"status": "failed"})

	if _, err := svc.Start(uuid.New(), uuid.New(), "ok.txt", 3, ""); err != nil {
		t.Fatalf("Start ok: %v", err)
	}
	if _, err := svc.Start(uuid.New(), uuid.New(), "bad", 1<<30, ""); !errors.Is(err, ErrSize) {
		t.Fatalf("Start oversize: err = %v, want ErrSize", err)
	}

	createdAfter := counterValue(t, "docflow_upload_sessions_total", map[string]string{"status": "created"})
	failedAfter := counterValue(t, "docflow_upload_sessions_total", map[string]string{"status": "failed"})
	if createdAfter-createdBefore != 1 {
		t.Fatalf("created 差值 = %v, want 1", createdAfter-createdBefore)
	}
	if failedAfter-failedBefore != 1 {
		t.Fatalf("failed 差值 = %v, want 1", failedAfter-failedBefore)
	}
}

// histogramCount 读取直方图指标 _count 系列当前值。
func histogramCount(t *testing.T, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			match := true
			for k, v := range labels {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// Complete 全链路：verify 阶段（SHA-256 校验）与 scan 阶段（CountingScanner）
// 各记一次 docflow_upload_processing_duration_seconds{stage}。
func TestCompleteObservesStageDurations(t *testing.T) {
	svc, store, _, id := testService(t, 3)
	svc.SetScanner(NewCountingScanner(AllowScanner{}))
	if _, err := svc.Append(id, 0, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	verifyBefore := histogramCount(t, "docflow_upload_processing_duration_seconds", map[string]string{"stage": "verify"})
	scanBefore := histogramCount(t, "docflow_upload_processing_duration_seconds", map[string]string{"stage": "scan"})
	if _, err := svc.Complete(id); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if v, _ := store.Get(id); v.Status != StatusAvailable {
		t.Fatalf("status = %s, want available", v.Status)
	}
	verifyAfter := histogramCount(t, "docflow_upload_processing_duration_seconds", map[string]string{"stage": "verify"})
	scanAfter := histogramCount(t, "docflow_upload_processing_duration_seconds", map[string]string{"stage": "scan"})
	if verifyAfter-verifyBefore != 1 {
		t.Fatalf("verify 观测差值 = %d, want 1", verifyAfter-verifyBefore)
	}
	if scanAfter-scanBefore != 1 {
		t.Fatalf("scan 观测差值 = %d, want 1", scanAfter-scanBefore)
	}
}
