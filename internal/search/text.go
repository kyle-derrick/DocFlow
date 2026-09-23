package search

import (
	"path/filepath"
	"strings"
)

// MaxContentBytes 内容索引的大小上限：超出仅索引名称（content=NULL）。
const MaxContentBytes = 2 << 20 // 2 MiB

// textExts 扩展名白名单：上传链路落库的 mime 恒为 application/octet-stream
// （upload.Service.Complete 不探测真实类型），文本判定主要依赖扩展名；
// 白名单覆盖常见文本与代码文件，二进制（图片/pdf/office）不在其列。
var textExts = map[string]bool{
	"md": true, "txt": true, "csv": true, "log": true, "json": true,
	"xml": true, "yml": true, "yaml": true, "js": true, "ts": true,
	"go": true, "py": true, "java": true, "c": true, "cpp": true,
	"h": true, "rs": true, "sql": true, "sh": true, "html": true, "css": true,
}

// textMimes 精确匹配的文本 mime（text/* 前缀另判）。
var textMimes = map[string]bool{
	"application/json": true,
	"application/xml":  true,
}

// IsTextIndexable 判定文件是否索引内容：mime 为 text/*、application/json、
// application/xml，或扩展名命中白名单（大小写不敏感）。二进制（图片/pdf/
// office）返回 false → 仅名称索引。
func IsTextIndexable(mime, name string) bool {
	m := strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	if strings.HasPrefix(m, "text/") || textMimes[m] {
		return true
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	return textExts[ext]
}

// imageExts 可 OCR 图片扩展名白名单：上传落库 mime 恒 application/
// octet-stream（upload.Service.Complete 不探测真实类型），图片判定与文本
// 判定一样主要依赖扩展名兜底；svg 为矢量文本格式，视觉 OCR 收益低且
// 与位图混流易误判，刻意不收。
var imageExts = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "webp": true,
	"gif": true, "bmp": true, "tiff": true,
}

// IsImageIndexable 判定 blob 是否为可 OCR 的图片：mime image/* 或常见
// 图片扩展名 jpg/jpeg/png/webp/gif/bmp/tiff（大小写不敏感，参照
// IsTextIndexable 的实现风格）。命中即允许索引管道走图片 OCR 兜底。
func IsImageIndexable(mimeType, name string) bool {
	m := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	if strings.HasPrefix(m, "image/") {
		return true
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	return imageExts[ext]
}

// EscapeLike 转义 ILIKE 模式元字符 %、_、\，使 q 作为字面子串匹配。
func EscapeLike(q string) string {
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		switch q[i] {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// LikePattern 返回 ILIKE 用的 %escaped% 子串模式（大小写不敏感由 ILIKE 保证）。
func LikePattern(q string) string {
	return "%" + EscapeLike(q) + "%"
}

// containsFold 内存实现的 ILIKE 子串匹配等价物（大小写不敏感；q 须为
// EscapeLike 后的字面串，此处直接字面包含判定）。
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
