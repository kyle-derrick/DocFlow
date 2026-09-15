package team

import (
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

// TestMemberTableName 验证 GORM schema 解析将 Member 映射到 team_members
// （复数化默认值是 members——运行时真实 PostgreSQL 才会暴露该问题）。
func TestMemberTableName(t *testing.T) {
	s, err := schema.Parse(&Member{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Table != "team_members" {
		t.Fatalf("Member table = %s, want team_members", s.Table)
	}
}
