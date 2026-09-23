package search

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

// fakeOCR 记录调用参数并返回预设结果（Indexer OCR 注入用 fake）。
type fakeOCR struct {
	calls []ocrCall
	text  string
	err   error
}

type ocrCall struct {
	key, mime, name string
	size            int64
}

func (f *fakeOCR) ExtractImageText(_ context.Context, key, mime, name string, size int64) (string, error) {
	f.calls = append(f.calls, ocrCall{key: key, mime: mime, name: name, size: size})
	return f.text, f.err
}

// TestIsImageIndexable 图片判定矩阵：image/* mime、常见图片扩展名（大小写
// 不敏感）、带参数 mime；非图片（pdf/zip/无扩展名/svg）不命中。
func TestIsImageIndexable(t *testing.T) {
	cases := []struct {
		mime, name string
		want       bool
		note       string
	}{
		{"image/png", "a", true, "image/* 前缀"},
		{"IMAGE/JPEG", "a", true, "mime 大小写不敏感"},
		{"image/webp; charset=binary", "a", true, "带参数的 image/*"},
		{"application/octet-stream", "photo.jpg", true, "上传落库 mime 恒 octet-stream，扩展名兜底：jpg"},
		{"application/octet-stream", "scan.PNG", true, "扩展名大小写不敏感"},
		{"application/octet-stream", "pic.webp", true, "扩展名：webp"},
		{"application/octet-stream", "old.bmp", true, "扩展名：bmp"},
		{"application/octet-stream", "doc.tiff", true, "扩展名：tiff"},
		{"application/pdf", "doc.pdf", false, "pdf 非图片"},
		{"application/zip", "pkg.zip", false, "zip 非图片"},
		{"application/octet-stream", "icon.svg", false, "svg 矢量文本，刻意不收"},
		{"", "noext", false, "无 mime 无扩展名"},
		{"text/plain", "a.txt", false, "文本已走内容索引，不重复 OCR"},
	}
	for _, c := range cases {
		if got := IsImageIndexable(c.mime, c.name); got != c.want {
			t.Errorf("IsImageIndexable(%q, %q) = %v, want %v (%s)", c.mime, c.name, got, c.want, c.note)
		}
	}
}

// TestIndexerOCRText OCR 注入语义：未注入返回空（行为不变）；注入后可 OCR
// 图片透传 blob 参数并回填文本；非图片不调用；提取失败降级空串。
func TestIndexerOCRText(t *testing.T) {
	ix := NewIndexer(NewStore(NewMemoryRepo()), nil, nil)
	blob := files.ObjectBlob{StorageKey: "img/1", MimeType: "image/png", Size: 10}
	// 未注入提取器：返回空、不 panic（旧版行为）。
	if got := ix.ocrText(uuid.New(), blob, "scan.png"); got != "" {
		t.Fatalf("未注入提取器应返回空: %q", got)
	}
	f := &fakeOCR{text: "发票 123"}
	ix.SetOCR(f)
	if got := ix.ocrText(uuid.New(), blob, "scan.png"); got != "发票 123" {
		t.Fatalf("ocr text = %q", got)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.calls))
	}
	if c := f.calls[0]; c.key != "img/1" || c.mime != "image/png" || c.name != "scan.png" || c.size != 10 {
		t.Fatalf("call = %+v（应透传 blob 参数）", c)
	}
	// octet-stream + 图片扩展名（上传链路实际形态）→ 命中兜底。
	if got := ix.ocrText(uuid.New(), files.ObjectBlob{StorageKey: "k", MimeType: "application/octet-stream", Size: 5}, "photo.JPG"); got != "发票 123" || len(f.calls) != 2 {
		t.Fatalf("扩展名兜底应命中: (%q, %d calls)", got, len(f.calls))
	}
	// 非图片（pdf）：不调用提取器。
	if got := ix.ocrText(uuid.New(), files.ObjectBlob{StorageKey: "k", MimeType: "application/pdf", Size: 5}, "doc.pdf"); got != "" || len(f.calls) != 2 {
		t.Fatalf("非图片不应调用: (%q, %d calls)", got, len(f.calls))
	}
	// 提取失败 → 空串（索引降级仅名称，错误由 Indexer 记日志）。
	f.err = errors.New("boom")
	if got := ix.ocrText(uuid.New(), blob, "scan.png"); got != "" {
		t.Fatalf("失败应返回空: %q", got)
	}
}
