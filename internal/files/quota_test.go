package files

import (
	"math"
	"testing"
)

// QuotaExceeded 判定矩阵（C3）：未超/恰好等于/超限、quota<=0 视为不限、
// int64 溢出按超限（fail closed）、非正 size 不判定超限。
func TestQuotaExceededMatrix(t *testing.T) {
	cases := []struct {
		name  string
		used  int64
		quota int64
		size  int64
		want  bool
	}{
		{"under-quota", 500, 1000, 400, false},
		{"exactly-quota", 900, 1000, 100, false},
		{"one-byte-over", 900, 1000, 101, true},
		{"zero-used-full-size", 0, 1000, 1000, false},
		{"zero-quota-unlimited", 500, 0, math.MaxInt64, false},
		{"overflow-fails-closed", math.MaxInt64, 1000, 1, true},
		{"zero-size", 100, 1000, 0, false},
		{"negative-size-defensive", 100, 1000, -5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QuotaExceeded(tc.used, tc.quota, tc.size); got != tc.want {
				t.Fatalf("QuotaExceeded(%d, %d, %d) = %v, want %v", tc.used, tc.quota, tc.size, got, tc.want)
			}
		})
	}
}
