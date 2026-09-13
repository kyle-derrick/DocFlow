package files

import "testing"

// effectiveMaxVersions 热读取语义：提供器优先，读取失败（提供器返回非正值，
// 调用方约定以 0/-1 作回退哨兵）或未注入时回退 SetMaxVersions 的静态值。
func TestEffectiveMaxVersionsHotReadAndFallback(t *testing.T) {
	s := NewStore(nil)
	s.SetMaxVersions(5)

	if got := s.effectiveMaxVersions(); got != 5 {
		t.Fatalf("no provider = %d, want 5", got)
	}

	// 提供器返回有效值：即时生效（每次调用都热读取）。
	value := 7
	s.SetMaxVersionsProvider(func() int { return value })
	if got := s.effectiveMaxVersions(); got != 7 {
		t.Fatalf("provider = %d, want 7", got)
	}
	value = 9
	if got := s.effectiveMaxVersions(); got != 9 {
		t.Fatalf("provider after change = %d, want 9 (hot read)", got)
	}

	// 提供器读取失败（返回 0）：回退静态值 5。
	value = 0
	if got := s.effectiveMaxVersions(); got != 5 {
		t.Fatalf("provider failure = %d, want fallback 5", got)
	}
	// 提供器返回负数（哨兵）：同样回退。
	value = -1
	if got := s.effectiveMaxVersions(); got != 5 {
		t.Fatalf("provider negative = %d, want fallback 5", got)
	}
}
