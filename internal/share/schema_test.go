package share

import (
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

// TestAccessSessionTableName 验证 GORM schema 解析将 AccessSession 映射到
// share_access_sessions（复数化默认值是 access_sessions——运行时真实
// PostgreSQL 才会暴露该问题，与 space.Member/user.UserTOTP 同型）。
func TestAccessSessionTableName(t *testing.T) {
	s, err := schema.Parse(&AccessSession{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Table != "share_access_sessions" {
		t.Fatalf("AccessSession table = %s, want share_access_sessions", s.Table)
	}
}

// TestShareAfterFindTrimsPasswordHash 验证 CHAR(64) 空格填充读值被清洗为
// 空串：无密码分享不得因填充空格被 HasPassword 误判（PostgreSQL CHAR 语义，
// 运行时冒烟暴露，与 upload.expected_sha256 同型陷阱）。
func TestShareAfterFindTrimsPasswordHash(t *testing.T) {
	sh := Share{PasswordHash: strings.Repeat(" ", 64)}
	if err := sh.AfterFind(nil); err != nil {
		t.Fatalf("AfterFind: %v", err)
	}
	if sh.HasPassword() {
		t.Fatal("passwordless share must not report HasPassword after AfterFind")
	}
}
