package xmind

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"
)

// buildXmind 构造内存 .xmind zip（文件名→内容）。
func buildXmind(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const twoSheetContentJSON = `[
  {
    "title": "工作计划",
    "rootTopic": {
      "title": "季度目标",
      "notes": {"plain": {"content": "根节点备注"}},
      "children": {"attached": [
        {"title": "研发", "children": {"attached": [
          {"title": "后端重构"},
          {"title": "前端优化", "notes": {"plain": {"content": "第一行\n第二行"}}}
        ]}},
        {"title": "市场"}
      ]}
    }
  },
  {
    "title": "会议记录",
    "rootTopic": {
      "title": "周会",
      "children": {"attached": [{"title": "待办清理"}]}
    }
  }
]`

func TestToMarkdownSheetsAndStructure(t *testing.T) {
	data := buildXmind(t, map[string]string{
		"content.json":             twoSheetContentJSON,
		"metadata.json":            `{"creator":{},"dataStructureVersion":"2"}`,
		"Thumbnails/thumbnail.png": "png",
	})
	got, err := ToMarkdown(data)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"# 工作计划",
		"",
		"- 季度目标",
		"  > 根节点备注",
		"  - 研发",
		"    - 后端重构",
		"    - 前端优化",
		"      > 第一行",
		"      > 第二行",
		"  - 市场",
		"",
		"# 会议记录",
		"",
		"- 周会",
		"  - 待办清理",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("markdown mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestToMarkdownSheetTitleFallback(t *testing.T) {
	// sheet 无 title：回退根 topic 标题。
	data := buildXmind(t, map[string]string{
		"content.json": `[{"rootTopic":{"title":"根标题"}}]`,
	})
	got, err := ToMarkdown(data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# 根标题") || !strings.Contains(got, "- 根标题") {
		t.Fatalf("fallback title missing: %q", got)
	}
}

func TestToMarkdownErrors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"not-a-zip", []byte("plain text, not a zip"), ErrInvalid},
		{"empty-zip", buildXmind(t, map[string]string{}), ErrInvalid},
		{"legacy-xml", buildXmind(t, map[string]string{"content.xml": "<xmap-content/>"}), ErrLegacyUnsupported},
		{"legacy-xml-with-json", buildXmind(t, map[string]string{
			"content.xml":  "<xmap-content/>",
			"content.json": `[{"title":"s","rootTopic":{"title":"r"}}]`,
		}), nil},
		{"bad-json", buildXmind(t, map[string]string{"content.json": "{not json"}), ErrInvalid},
		{"empty-sheets", buildXmind(t, map[string]string{"content.json": "[]"}), ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ToMarkdown(tc.data)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
