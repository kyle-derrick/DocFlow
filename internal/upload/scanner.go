package upload

import (
	"errors"
	"io"
	"time"

	"github.com/docflow/docflow/internal/metrics"
)

type Scanner interface{ Scan(io.Reader) error }
type AllowScanner struct{}

func (AllowScanner) Scan(io.Reader) error { return nil }

type RejectScanner struct{}

func (RejectScanner) Scan(io.Reader) error { return io.ErrUnexpectedEOF }

// CountingScanner 包装内层 Scanner，把扫描结果与耗时计入 Prometheus 指标：
//   - docflow_scan_results_total{result=clean|infected|error}：
//     nil -> clean；errors.Is(err, ErrMalwareDetected) -> infected；
//     其余错误（clamd 不可达、协议异常、RejectScanner fail-closed 等）-> error；
//   - docflow_upload_processing_duration_seconds{stage=scan}。
//
// 包装不改变内层行为与错误语义，可在接线处套在 SelectScanner 结果之外。
type CountingScanner struct {
	next Scanner
}

// NewCountingScanner 包装 scanner（nil 时回退 AllowScanner，保持可用）。
func NewCountingScanner(scanner Scanner) Scanner {
	if scanner == nil {
		scanner = AllowScanner{}
	}
	return &CountingScanner{next: scanner}
}

func (s *CountingScanner) Scan(r io.Reader) error {
	start := time.Now()
	err := s.next.Scan(r)
	metrics.ObserveUploadProcessing(metrics.UploadStageScan, time.Since(start).Seconds())
	switch {
	case err == nil:
		metrics.IncScanResult(metrics.ScanResultClean)
	case errors.Is(err, ErrMalwareDetected):
		metrics.IncScanResult(metrics.ScanResultInfected)
	default:
		metrics.IncScanResult(metrics.ScanResultError)
	}
	return err
}
