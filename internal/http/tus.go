package http

import (
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// tus 1.0.0 核心协议 + creation/expiration 扩展，基于 upload.Service 自实现（不引入 tusd）。
// 不支持 concatenation / termination / checksum 扩展。
const (
	tusVersion        = "1.0.0"
	tusExtension      = "creation,expiration"
	tusBasePath       = "/api/v1/tus/files/"
	tusPatchMediaType = "application/offset+octet-stream"
)

// registerTUSRoutes 在认证路由组下挂载 tus 端点；所有响应统一带 Tus-Resumable: 1.0.0。
func registerTUSRoutes(g *gin.RouterGroup, h *Handler) {
	g.Use(func(c *gin.Context) {
		c.Header("Tus-Resumable", tusVersion)
		c.Next()
	})
	g.OPTIONS("/files", h.tusOptions)
	g.POST("/files", h.tusCreate)
	g.HEAD("/files/:id", h.tusHead)
	g.PATCH("/files/:id", h.tusPatch)
}

// parseTusMetadata 解析 Upload-Metadata 头（纯函数，便于单测）。
// 格式：逗号分隔的 "key base64value" 对，value 可整体省略；
// 空头返回空 map；任一对为空、含多余字段或 value 非法 base64 均报错。
func parseTusMetadata(header string) (map[string]string, error) {
	meta := make(map[string]string)
	if strings.TrimSpace(header) == "" {
		return meta, nil
	}
	for _, pair := range strings.Split(header, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			return nil, errors.New("empty metadata pair")
		}
		parts := strings.Fields(pair)
		if len(parts) > 2 {
			return nil, errors.New("malformed metadata pair")
		}
		value := ""
		if len(parts) == 2 {
			decoded, err := base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				return nil, err
			}
			value = string(decoded)
		}
		meta[parts[0]] = value
	}
	return meta, nil
}

// tusError 以简短 JSON body 返回 tus 风格错误（Tus-Resumable 由组中间件统一附加）。
func tusError(c *gin.Context, code int, message string) {
	c.JSON(code, gin.H{"error": message})
}

// tusOptions 处理 OPTIONS /api/v1/tus/files：宣告能力。
func (h *Handler) tusOptions(c *gin.Context) {
	c.Header("Tus-Version", tusVersion)
	c.Header("Tus-Extension", tusExtension)
	if max := h.uploads.MaxSize(); max > 0 {
		c.Header("Tus-Max-Size", strconv.FormatInt(max, 10))
	}
	c.Status(http.StatusNoContent)
}

// tusCreate 处理 POST /api/v1/tus/files（creation 扩展）。
func (h *Handler) tusCreate(c *gin.Context) {
	length, err := strconv.ParseInt(c.GetHeader("Upload-Length"), 10, 64)
	if err != nil || length < 0 {
		tusError(c, http.StatusBadRequest, "invalid or missing Upload-Length")
		return
	}
	if max := h.uploads.MaxSize(); max > 0 && length > max {
		tusError(c, http.StatusRequestEntityTooLarge, "upload length exceeds Tus-Max-Size")
		return
	}
	rawMeta := c.GetHeader("Upload-Metadata")
	meta, err := parseTusMetadata(rawMeta)
	if err != nil {
		tusError(c, http.StatusBadRequest, "invalid Upload-Metadata")
		return
	}
	name := meta["filename"]
	if name == "" {
		tusError(c, http.StatusBadRequest, "Upload-Metadata must include filename")
		return
	}
	parent := uuid.Nil
	if raw := meta["parent_id"]; raw != "" {
		parent, err = uuid.Parse(raw)
		if err != nil {
			tusError(c, http.StatusBadRequest, "invalid parent_id metadata")
			return
		}
	} else {
		root, e := h.files.EnsureRoot(userID(c))
		if e != nil {
			tusError(c, http.StatusInternalServerError, "unable to ensure root folder")
			return
		}
		parent = root.ID
	}
	// file_id metadata（可选）：覆盖为新版本会话；此时 filename/parent_id 被忽略，
	// 会话沿用目标文件现有名称与父目录。
	var v upload.UploadSession
	if raw := meta["file_id"]; raw != "" {
		target, e := uuid.Parse(raw)
		if e != nil {
			tusError(c, http.StatusBadRequest, "invalid file_id metadata")
			return
		}
		v, err = h.uploads.StartReplace(userID(c), target, length, meta["sha256"])
	} else {
		v, err = h.uploads.Start(userID(c), parent, name, length, meta["sha256"])
	}
	if err != nil {
		switch {
		case errors.Is(err, upload.ErrSize):
			tusError(c, http.StatusRequestEntityTooLarge, err.Error())
		case errors.Is(err, files.ErrForbidden), errors.Is(err, upload.ErrTargetUnavailable):
			// 团队目录写权限不足（viewer/非成员）或版本覆盖能力未接线。
			tusError(c, http.StatusForbidden, err.Error())
		case errors.Is(err, files.ErrNotFound):
			// 目标文件不存在（或个人文件非 owner）：404 不泄露存在性。
			tusError(c, http.StatusNotFound, "file not found")
		case errors.Is(err, upload.ErrHash), errors.Is(err, files.ErrInvalidName), errors.Is(err, files.ErrInvalidTarget):
			tusError(c, http.StatusBadRequest, err.Error())
		default:
			tusError(c, http.StatusInternalServerError, "unable to create upload")
		}
		return
	}
	if rawMeta != "" {
		if e := h.uploads.SetMetadata(v.ID, rawMeta); e != nil {
			tusError(c, http.StatusInternalServerError, "unable to persist upload metadata")
			return
		}
		v.Metadata = rawMeta
	}
	c.Header("Location", tusBasePath+v.ID.String())
	c.Header("Upload-Offset", "0")
	c.Header("Upload-Expires", v.ExpiresAt.UTC().Format(http.TimeFormat))
	c.Status(http.StatusCreated)
}

// tusSession 加载 :id 对应会话；解析失败、不存在或非本人资源均视为未找到（不泄露存在性）。
// 响应由调用方按各自协议语义写出。
func (h *Handler) tusSession(c *gin.Context) (upload.UploadSession, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return upload.UploadSession{}, false
	}
	v, err := h.uploads.Get(id)
	if err != nil || v.UserID != userID(c) {
		return upload.UploadSession{}, false
	}
	return v, true
}

// tusHead 处理 HEAD /api/v1/tus/files/:id：HEAD 响应不得携带 body。
func (h *Handler) tusHead(c *gin.Context) {
	v, ok := h.tusSession(c)
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	if v.Status == upload.StatusUploading && time.Now().After(v.ExpiresAt) {
		c.Status(http.StatusGone)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Upload-Offset", strconv.FormatInt(v.Offset, 10))
	c.Header("Upload-Length", strconv.FormatInt(v.Size, 10))
	if v.Metadata != "" {
		c.Header("Upload-Metadata", v.Metadata)
	}
	c.Status(http.StatusOK)
}

// tusPatch 处理 PATCH /api/v1/tus/files/:id：按 offset 追加 body。
func (h *Handler) tusPatch(c *gin.Context) {
	if c.GetHeader("Content-Type") != tusPatchMediaType {
		tusError(c, http.StatusUnsupportedMediaType, "Content-Type must be application/offset+octet-stream")
		return
	}
	v, ok := h.tusSession(c)
	if !ok {
		tusError(c, http.StatusNotFound, "upload not found")
		return
	}
	switch v.Status {
	case upload.StatusAvailable, upload.StatusQuarantined, upload.StatusFailed:
		tusError(c, http.StatusGone, "upload is in terminal state")
		return
	}
	offset, err := strconv.ParseInt(c.GetHeader("Upload-Offset"), 10, 64)
	if err != nil {
		tusError(c, http.StatusBadRequest, "invalid Upload-Offset")
		return
	}
	if offset != v.Offset {
		tusError(c, http.StatusConflict, "Upload-Offset does not match current offset")
		return
	}
	v, err = h.uploads.Append(v.ID, offset, c.Request.Body)
	if err != nil {
		switch {
		case errors.Is(err, upload.ErrExpired):
			tusError(c, http.StatusGone, "upload session expired")
		case errors.Is(err, upload.ErrOffset):
			tusError(c, http.StatusConflict, err.Error())
		case errors.Is(err, upload.ErrSize):
			tusError(c, http.StatusBadRequest, "request body exceeds remaining upload length")
		default:
			tusError(c, http.StatusInternalServerError, "upload failed")
		}
		return
	}
	c.Header("Upload-Offset", strconv.FormatInt(v.Offset, 10))
	c.Header("Upload-Expires", v.ExpiresAt.UTC().Format(http.TimeFormat))
	c.Status(http.StatusNoContent)
	if v.Offset == v.Size {
		// 自动完成：后台异步执行 verify→scan→available，避免 Complete 的
		// 哈希/扫描/落盘阻塞 PATCH 响应；状态可轮询 GET /api/v1/uploads/:id。
		h.enqueueCompleteUpload(v)
	}
}

// enqueueCompleteUpload 派发后台补完任务：注入 Enqueuer 时经队列执行
// （inprocess 驱动与原内联 goroutine 行为一致；redis 驱动由任意实例的
// worker 处理）；未注入或入队失败（如 Redis 抖动）回退进程内 goroutine，
// Complete 对终态幂等，重复执行安全。
func (h *Handler) enqueueCompleteUpload(v upload.UploadSession) {
	if h.tasks == nil {
		go h.tusAutocomplete(v)
		return
	}
	if err := h.tasks.EnqueueCompleteUpload(v.ID); err != nil {
		log.Printf("[tus] enqueue complete-upload %s: %v (fallback inline)", v.ID, err)
		go h.tusAutocomplete(v)
	}
}

// tusAutocomplete 后台补完上传并记录审计；Complete 对终态幂等，重复触发安全。
func (h *Handler) tusAutocomplete(v upload.UploadSession) {
	if _, err := h.uploads.Complete(v.ID); err != nil {
		return
	}
	owner := v.UserID
	_ = h.audit.Record(audit.Entry{UserID: &owner, Action: audit.ActionUploadComplete, ResourceType: audit.ResourceUpload, ResourceID: v.ID.String(), Status: audit.StatusSuccess, Metadata: `{"name":"` + sanitizeAuditToken(v.Name) + `","size":` + strconv.FormatInt(v.Size, 10) + `}`})
	// 覆盖为新版本的会话：另记一条 version.create（资源为目标文件）。
	if v.TargetFileID != nil {
		_ = h.audit.Record(audit.Entry{UserID: &owner, Action: audit.ActionVersionCreate, ResourceType: audit.ResourceFile, ResourceID: v.TargetFileID.String(), Status: audit.StatusSuccess, Metadata: `{"upload_id":"` + v.ID.String() + `","size":` + strconv.FormatInt(v.Size, 10) + `}`})
	}
}
