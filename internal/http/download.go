package http

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/docflow/docflow/internal/files"
)

type byteRange struct {
	Start int64
	End   int64 // 含端点
}

func (r byteRange) Length() int64 { return r.End - r.Start + 1 }

// parseRange 解析 "Range: bytes=start-end" 头。
// 返回 ok=false 表示无 Range 头（返回 200 全量）；
// 返回 err 非 nil 表示 Range 语义无效或不可满足（返回 416）。
// 仅支持单个区间，多区间请求视为无效（按任务约定“简单 Range”）。
func parseRange(header string, size int64) (byteRange, bool, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return byteRange{}, false, nil
	}
	const prefix = "bytes="
	if !strings.HasPrefix(strings.ToLower(header), prefix) {
		return byteRange{}, false, fmt.Errorf("unsupported range unit")
	}
	spec := strings.TrimSpace(header[len(prefix):])
	if spec == "" || strings.Contains(spec, ",") {
		return byteRange{}, false, fmt.Errorf("invalid range")
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return byteRange{}, false, fmt.Errorf("invalid range")
	}
	startText, endText := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])

	var r byteRange
	if startText == "" {
		// 后缀区间 bytes=-N：最后 N 字节。
		n, err := strconv.ParseInt(endText, 10, 64)
		if err != nil || n < 0 {
			return byteRange{}, false, fmt.Errorf("invalid suffix range")
		}
		if n == 0 {
			return byteRange{}, false, fmt.Errorf("empty suffix range")
		}
		if size == 0 {
			return byteRange{}, false, fmt.Errorf("range not satisfiable")
		}
		r.Start = size - n
		if r.Start < 0 {
			r.Start = 0
		}
		r.End = size - 1
		return r, true, nil
	}
	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil || start < 0 {
		return byteRange{}, false, fmt.Errorf("invalid range start")
	}
	if start >= size {
		return byteRange{}, false, fmt.Errorf("range not satisfiable")
	}
	r.Start = start
	if endText == "" {
		r.End = size - 1
		return r, true, nil
	}
	end, err := strconv.ParseInt(endText, 10, 64)
	if err != nil || end < start {
		return byteRange{}, false, fmt.Errorf("invalid range end")
	}
	if end >= size {
		end = size - 1
	}
	r.End = end
	return r, true, nil
}

// contentDisposition 生成下载响应的 Content-Disposition 值，文件名按 RFC 5987 编码，
// 非 ASCII 文件名放在 filename*=UTF-8” 扩展参数中，避免响应头损坏。
// 同时提供 ASCII 回退值（filename=，quoted-string 形式），供旧客户端使用。
func contentDisposition(filename string) string {
	return dispositionValue("attachment", filename)
}

// inlineDisposition 生成内联预览响应的 Content-Disposition: inline 值，编码规则与下载一致。
func inlineDisposition(filename string) string {
	return dispositionValue("inline", filename)
}

func dispositionValue(kind, filename string) string {
	fallback, isPlainASCII := asciiFallback(filename)
	disposition := kind + `; filename="` + fallback + `"`
	if !isPlainASCII {
		disposition += `; filename*=UTF-8''` + rfc5987Encode(filename)
	}
	return disposition
}

// asciiFallback 返回可安全放入 quoted-string 的 ASCII 文件名：
// 保留可打印 ASCII（0x20-0x7E，引号与反斜杠按 quoted-string 规则转义），
// 其余字符（非 ASCII、控制字符）按 rune 替换为下划线。
// isPlainASCII 表示原文无需任何替换即可安全使用。
func asciiFallback(filename string) (string, bool) {
	var b strings.Builder
	b.Grow(len(filename))
	changed := false
	for _, r := range filename {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
			changed = true
		case r >= 0x20 && r <= 0x7E:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
			changed = true
		}
	}
	if !changed {
		return filename, true
	}
	return b.String(), false
}

const upperhex = "0123456789ABCDEF"

// rfc5987Encode 按 RFC 5987 对文件名做 percent-encoding：
// 仅保留 attr-char（字母数字及 !#$&+-.^_`|~），其余字节（含空格与控制字符）编码为 %XX。
func rfc5987Encode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case strings.IndexByte("!#$&+-.^_`|~", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0xF])
		}
	}
	return b.String()
}

func (h *Handler) getFile(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	owner := userID(c)
	f, err := h.files.Get(owner, id)
	if h.fileError(c, err) {
		return
	}
	out := fileJSON(f)
	if version, blob, err := h.files.CurrentVersion(owner, id); err == nil {
		out["current_version"] = gin.H{
			"id":        version.ID,
			"version":   version.Version,
			"size":      blob.Size,
			"sha256":    blob.SHA256,
			"mime_type": blob.MimeType,
			"status":    blob.Status,
		}
	} else {
		out["current_version"] = nil
	}
	setETag(c, f)
	c.JSON(http.StatusOK, out)
}

func (h *Handler) downloadFile(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	owner := userID(c)
	_, blob, err := h.files.CurrentVersion(owner, id)
	if err != nil {
		h.fileError(c, err)
		return
	}
	if blob.Status != files.BlobStatusAvailable {
		c.JSON(http.StatusForbidden, gin.H{"error": "file is not available for download", "status": blob.Status})
		return
	}
	f, err := h.files.Get(owner, id)
	if err != nil {
		h.fileError(c, err)
		return
	}
	r, hasRange, err := parseRange(c.GetHeader("Range"), blob.Size)
	if err != nil {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", blob.Size))
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}
	file, err := h.storage.Read(blob.StorageKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	defer file.Close()
	seeker, ok := file.(io.Seeker)
	if hasRange && !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	if err := h.files.IncrementDownloadCount(owner, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to record download"})
		return
	}
	status := http.StatusOK
	contentLength := blob.Size
	var reader io.Reader = file
	if hasRange {
		status = http.StatusPartialContent
		contentLength = r.Length()
		if _, serr := seeker.Seek(r.Start, io.SeekStart); serr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
			return
		}
		reader = io.LimitReader(file, r.Length())
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", r.Start, r.End, blob.Size))
	}
	c.Header("Content-Disposition", contentDisposition(f.Name))
	c.Header("Accept-Ranges", "bytes")
	c.DataFromReader(status, contentLength, blob.MimeType, reader, nil)
}
