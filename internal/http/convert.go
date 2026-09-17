package http

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/xmind"
)

// convertMaxSourceSize 为待转换源文件（.xmind）的读取上限（防御异常超大对象
// 占用内存；超出按解析失败处理）。
const convertMaxSourceSize = 64 << 20

// convertSuffixBase 为目标名冲突时的自动后缀基名（-converted-N，N 从 1 起）。
const convertSuffixBase = "-converted"

// convertToMarkdown POST /api/v1/files/:id/convert-markdown：把 .xmind 文件
// 解析为 Markdown（internal/xmind，新格式 content.json；老版 content.xml
// 明确不支持），经 UploadBytes 管线落库到源文件所在目录（校验/配额自然
// 生效），返回新 file_id。目标名 <同名>.md，冲突自动加 -converted-N 后缀。
// 源非 .xmind 或解析失败返回 422（附原因）；写权限/配额错误沿用上传映射
// （403/404 等）。
func (h *Handler) convertToMarkdown(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	f, err := h.unpacker.Get(actor, id)
	if h.fileError(c, err) {
		return
	}
	if !strings.EqualFold(filepath.Ext(f.Name), ".xmind") {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "source must be an .xmind file", "code": "CONVERT_INVALID"})
		return
	}
	if f.ParentID == nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "source file has no parent folder", "code": "CONVERT_INVALID"})
		return
	}
	_, blob, err := h.unpacker.CurrentVersion(actor, id)
	if h.fileError(c, err) {
		return
	}
	if blob.Status != files.BlobStatusAvailable {
		c.JSON(http.StatusForbidden, gin.H{"error": "file is not available", "status": blob.Status})
		return
	}
	data, err := readSourceBlob(h, blob)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read source file"})
		return
	}
	markdown, err := xmind.ToMarkdown(data)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "code": "CONVERT_FAILED"})
		return
	}
	name, err := h.uniqueConvertName(*f.ParentID, f.Name)
	if err != nil {
		h.fileError(c, err)
		return
	}
	session, err := h.uploads.UploadBytes(actor, *f.ParentID, name, []byte(markdown))
	if err != nil {
		uploadStartError(c, err, false)
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: "file.convert_markdown", ResourceType: audit.ResourceFile, ResourceID: f.ID.String(), Metadata: fmt.Sprintf(`{"target_file_id":"%s","name":"%s","size":%d}`, session.FileID, sanitizeAuditToken(name), len(markdown))})
	c.JSON(http.StatusCreated, gin.H{"file_id": session.FileID, "name": name, "size": len(markdown)})
}

// readSourceBlob 从对象存储读取源内容（上限 convertMaxSourceSize；
// 长度与 blob.Size 校验一致）。
func readSourceBlob(h *Handler, blob files.ObjectBlob) ([]byte, error) {
	rc, err := h.storage.Read(blob.StorageKey)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, convertMaxSourceSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > convertMaxSourceSize || int64(len(data)) != blob.Size {
		return nil, errors.New("source content size mismatch")
	}
	return data, nil
}

// uniqueConvertName 生成目标文件名：<源名去扩展>.md；同目录重名时追加
// -converted-N（N 从 1 递增至不冲突，上限 100 后返回冲突错误）。
func (h *Handler) uniqueConvertName(parent uuid.UUID, sourceName string) (string, error) {
	base := strings.TrimSuffix(sourceName, filepath.Ext(sourceName))
	candidates := []string{base + ".md"}
	for i := 1; i <= 100; i++ {
		candidates = append(candidates, fmt.Sprintf("%s%s-%d.md", base, convertSuffixBase, i))
	}
	for _, name := range candidates {
		_, err := h.unpacker.FindChildByName(parent, name)
		if errors.Is(err, files.ErrNotFound) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", files.ErrConflict
}
