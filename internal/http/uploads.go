package http

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type uploadRequest struct {
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	ExpectedSHA256 string `json:"expected_sha256"`
	ParentID       string `json:"parent_id"`
	// FileID 可选：指定时为「覆盖为新版本」会话，上传内容作为该文件的新版本
	//（须 CanWrite；name/parent_id 被忽略，沿用目标文件现有名称与父目录）。
	FileID string `json:"file_id"`
}

// uploadStartError 统一映射会话创建错误（自定义 API 与 tus 共用语义）。
func uploadStartError(c *gin.Context, err error, tus bool) {
	badRequest := func(message string) {
		if tus {
			tusError(c, http.StatusBadRequest, message)
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": message})
	}
	switch {
	case errors.Is(err, files.ErrForbidden), errors.Is(err, upload.ErrTargetUnavailable):
		if tus {
			tusError(c, http.StatusForbidden, err.Error())
		} else {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		}
	case errors.Is(err, files.ErrNotFound):
		// 目标文件不存在（或个人文件非 owner）：404 不泄露存在性。
		if tus {
			tusError(c, http.StatusNotFound, "file not found")
		} else {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		}
	case errors.Is(err, files.ErrInvalidTarget):
		badRequest(err.Error())
	case errors.Is(err, upload.ErrSize):
		if tus {
			tusError(c, http.StatusRequestEntityTooLarge, err.Error())
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
	default:
		badRequest(err.Error())
	}
}

func (h *Handler) createUpload(c *gin.Context) {
	var req uploadRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	var v upload.UploadSession
	if req.FileID != "" {
		target, ok := parseID(c, req.FileID)
		if !ok {
			return
		}
		var err error
		v, err = h.uploads.StartReplace(userID(c), target, req.Size, req.ExpectedSHA256)
		if err != nil {
			uploadStartError(c, err, false)
			return
		}
	} else {
		parent := uuid.Nil
		if req.ParentID == "" {
			root, err := h.files.EnsureRoot(userID(c))
			if err != nil {
				c.JSON(500, gin.H{"error": "unable to ensure root folder"})
				return
			}
			parent = root.ID
		} else {
			var ok bool
			parent, ok = parseID(c, req.ParentID)
			if !ok {
				return
			}
		}
		var err error
		v, err = h.uploads.Start(userID(c), parent, req.Name, req.Size, req.ExpectedSHA256)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	c.Header("Location", "/api/v1/uploads/"+v.ID.String())
	c.JSON(http.StatusCreated, v)
}
func (h *Handler) patchUpload(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(c.GetHeader("Upload-Offset"), 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid offset"})
		return
	}
	v, err := h.uploads.Append(id, offset, c.Request.Body)
	if errors.Is(err, upload.ErrOffset) || errors.Is(err, upload.ErrSize) {
		c.JSON(409, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(500, gin.H{"error": "upload failed"})
		return
	}
	c.Header("Upload-Offset", strconv.FormatInt(v.Offset, 10))
	c.JSON(http.StatusOK, v)
}
func (h *Handler) completeUpload(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	current, err := h.uploads.Get(id)
	if err != nil {
		c.JSON(404, gin.H{"error": "upload not found"})
		return
	}
	// 终态幂等：重复调用 complete 直接返回当前状态，不重新处理，也不掩盖错误。
	switch current.Status {
	case upload.StatusAvailable:
		c.JSON(http.StatusOK, current)
		return
	case upload.StatusQuarantined:
		c.JSON(409, gin.H{"error": "scan rejected", "code": "QUARANTINED", "status": current.Status})
		return
	case upload.StatusFailed:
		c.JSON(409, gin.H{"error": "upload failed", "code": "FAILED", "status": current.Status})
		return
	}
	v, err := h.uploads.Complete(id)
	if err != nil {
		c.JSON(409, gin.H{"error": err.Error(), "status": v.Status})
		return
	}
	owner := userID(c)
	h.recordAudit(c, audit.Entry{UserID: &owner, Action: audit.ActionUploadComplete, ResourceType: audit.ResourceUpload, ResourceID: id.String(), Status: audit.StatusSuccess, Metadata: `{"name":"` + sanitizeAuditToken(v.Name) + `","size":` + strconv.FormatInt(v.Size, 10) + `}`})
	// 覆盖为新版本的会话：另记一条 version.create（资源为目标文件）。
	if v.TargetFileID != nil {
		h.recordAudit(c, audit.Entry{UserID: &owner, Action: audit.ActionVersionCreate, ResourceType: audit.ResourceFile, ResourceID: v.TargetFileID.String(), Status: audit.StatusSuccess, Metadata: `{"upload_id":"` + id.String() + `","size":` + strconv.FormatInt(v.Size, 10) + `}`})
	}
	c.JSON(http.StatusOK, v)
}
func (h *Handler) getUpload(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	v, err := h.uploads.Get(id)
	if err != nil {
		c.JSON(404, gin.H{"error": "upload not found"})
		return
	}
	out := gin.H{"id": v.ID, "parent_id": v.ParentID, "name": v.Name, "size": v.Size, "offset": v.Offset, "status": v.Status, "expires_at": v.ExpiresAt, "created_at": v.CreatedAt, "completed_at": v.CompletedAt}
	if v.TargetFileID != nil {
		out["file_id"] = v.TargetFileID
	}
	c.JSON(http.StatusOK, out)
}
