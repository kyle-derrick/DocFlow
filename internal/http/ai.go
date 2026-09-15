package http

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/upload"
)

// aiSummarizer 抽象 AI 摘要客户端（SetAI 注入；生产实现 *ai.Client）：
// Enabled=false 或未注入时端点 503 AI_DISABLED。
type aiSummarizer interface {
	Enabled() bool
	Summarize(ctx context.Context, text, filename string) (string, error)
}

// aiFileSource 抽象 AI 摘要所需的文件读取源（NewHandler 以 *files.Store 装配，
// 接口化便于单测注入内存实现；读授权复用 files.Get/CurrentVersion 的
// authorizeFileAccess 语义）。
type aiFileSource interface {
	Get(user, fileID uuid.UUID) (files.File, error)
	CurrentVersion(user, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
}

// fileAISummary POST /api/v1/files/:id/ai/summary：生成文件当前版本内容的
// AI 摘要（OpenAI 兼容 /chat/completions）。
//   - 读权限：authorizeFileAccess 同规则（个人 owner / 团队在册成员 + 路径级 ACL）；
//   - 文本类判定复用 search.IsTextIndexable 语义（mime text/*、json/xml、
//     扩展名白名单），非文本 400；
//   - 当前版本 blob 须 available（其余状态 409）；
//   - 内容 >100KB 拒绝 413 CONTENT_TOO_LARGE（不截断，避免误导性摘要）；
//   - AI 禁用 503 AI_DISABLED；上游失败（含超时）502；
//   - 高频端点：不记录审计。
func (h *Handler) fileAISummary(c *gin.Context) {
	if h.ai == nil || !h.ai.Enabled() || h.aiFiles == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai summary is disabled", "code": "AI_DISABLED"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	user := userID(c)
	f, err := h.aiFiles.Get(user, id)
	if h.fileError(c, err) {
		return
	}
	if f.Type != "file" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a text file", "code": "NOT_TEXT_FILE"})
		return
	}
	_, blob, err := h.aiFiles.CurrentVersion(user, id)
	if h.fileError(c, err) {
		return
	}
	if blob.Status != files.BlobStatusAvailable {
		c.JSON(http.StatusConflict, gin.H{"error": "file version is not available"})
		return
	}
	if !search.IsTextIndexable(blob.MimeType, f.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a text file", "code": "NOT_TEXT_FILE"})
		return
	}
	if blob.Size > ai.MaxInputBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "content too large for ai summary", "code": "CONTENT_TOO_LARGE"})
		return
	}
	text, err := readAISummaryInput(h.storage, blob.StorageKey)
	if err != nil {
		if errors.Is(err, ai.ErrTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "content too large for ai summary", "code": "CONTENT_TOO_LARGE"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file content"})
		return
	}
	summary, err := h.ai.Summarize(c.Request.Context(), text, f.Name)
	switch {
	case err == nil:
	case errors.Is(err, ai.ErrTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "content too large for ai summary", "code": "CONTENT_TOO_LARGE"})
		return
	case errors.Is(err, ai.ErrDisabled):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai summary is disabled", "code": "AI_DISABLED"})
		return
	case errors.Is(err, ai.ErrUpstream):
		c.JSON(http.StatusBadGateway, gin.H{"error": "ai upstream request failed"})
		return
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ai summary failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary})
}

// readAISummaryInput 读取 blob 内容（防御性限流读，超限按内容过长拒绝）。
func readAISummaryInput(storage upload.Storage, key string) (string, error) {
	r, err := storage.Read(key)
	if err != nil {
		return "", err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, ai.MaxInputBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > ai.MaxInputBytes {
		return "", ai.ErrTooLarge
	}
	return string(data), nil
}
