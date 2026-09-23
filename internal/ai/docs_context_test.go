// Package ai —— docs_context_test.go：DocsContextBlock（include_docs 的
//「我的文件」上下文拼装）单测——file 过滤/编号/去高亮标记/空结果空串。
package ai

import (
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/search"
	"github.com/google/uuid"
)

func TestDocsContextBlock(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	results := []search.Result{
		{ID: id1, Type: "file", Name: "规格说明.md", Snippet: "[[登录]]流程说明"},
		{ID: uuid.New(), Type: "folder", Name: "目录不该出现"},
		{ID: id2, Type: "file", Name: "接口文档.md", Snippet: "  "},
	}
	block, sources := DocsContextBlock(results)
	if len(sources) != 2 {
		t.Fatalf("sources len = %d, want 2（folder 过滤）", len(sources))
	}
	if sources[0].FileID != id1.String() || sources[0].URL != "/view/"+id1.String() {
		t.Fatalf("sources[0] = %+v", sources[0])
	}
	if !strings.Contains(block, "文档检索结果") || !strings.Contains(block, "[1] 文件名：规格说明.md") || !strings.Contains(block, "[2] 文件名：接口文档.md") {
		t.Fatalf("block 缺少拼装要素：%q", block)
	}
	if strings.Contains(block, "[[") || strings.Contains(block, "]]") {
		t.Fatalf("block 未去 ts_headline 高亮标记：%q", block)
	}
	if strings.Contains(block, "目录不该出现") {
		t.Fatalf("block 不应包含非 file 结果")
	}
	// 空输入/仅目录 → 空串空来源（调用方跳过注入）。
	if b, s := DocsContextBlock(nil); b != "" || len(s) != 0 {
		t.Fatalf("nil 输入应返回空：%q %v", b, s)
	}
	if b, s := DocsContextBlock([]search.Result{{ID: uuid.New(), Type: "folder", Name: "d"}}); b != "" || len(s) != 0 {
		t.Fatalf("仅目录输入应返回空：%q %v", b, s)
	}
}
