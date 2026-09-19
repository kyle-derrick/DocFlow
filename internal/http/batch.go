package http

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/settings"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// batchMaxItems 单次批量操作上限的回退值（settings 读取失败/未注入时）；
// 运行时经 system_settings 的 batch.max_items 热读取覆盖（设计 7.9
// BATCH_OPERATION_MAX_ITEMS=100）。
const batchMaxItems = 100

// batchLimit 返回当前生效的批量操作上限：settings 热读取优先（每次请求），
// 读取失败或非法值（<1）时回退 batchMaxItems。
func (h *Handler) batchLimit() int {
	if h.settings == nil {
		return batchMaxItems
	}
	if n, err := h.settings.GetInt(settings.KeyBatchMaxItems); err == nil && n >= 1 {
		return n
	}
	return batchMaxItems
}

// idempotencyTTL 幂等键缓存有效期（同 key+同 body hash 在窗口内重放缓存响应）。
const idempotencyTTL = 60 * time.Second

// idemPurgeThreshold 触发过期清理的条目数阈值（防内存无界增长）。
const idemPurgeThreshold = 1024

// idemEntry 幂等缓存条目：同 key 重复请求按 body hash 判定重放或拒绝。
type idemEntry struct {
	bodyHash  string
	status    int
	body      []byte
	expiresAt time.Time
}

// idemCache 进程内幂等响应缓存（mutex+map；get/put 均懒清理过期项）。
// 多实例局限：缓存不跨实例共享，负载均衡把同 key 重试打到其他实例时
// 幂等不生效（重试会被当作新请求执行）；单实例/粘性路由下语义完整。
// 跨实例一致需引入共享存储（Redis 等），v1.0 不引入。
type idemCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]idemEntry
}

func newIdemCache(ttl time.Duration) *idemCache {
	return &idemCache{ttl: ttl, entries: map[string]idemEntry{}}
}

func (c *idemCache) get(key, bodyHash string) (idemEntry, bool, bool) {
	// 返回 (entry, hit, hashMatch)：hit 表示 key 命中且未过期。
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return idemEntry{}, false, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.entries, key)
		return idemEntry{}, false, false
	}
	return e, true, e.bodyHash == bodyHash
}

func (c *idemCache) put(key, bodyHash string, status int, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= idemPurgeThreshold {
		now := time.Now()
		for k, e := range c.entries {
			if now.After(e.expiresAt) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = idemEntry{bodyHash: bodyHash, status: status, body: body, expiresAt: time.Now().Add(c.ttl)}
}

// replayWriter 透传写响应并捕获副本，供幂等缓存回放。
type replayWriter struct {
	gin.ResponseWriter
	buf bytes.Buffer
}

func (w *replayWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

// idempotency 为批量端点提供可选 Idempotency-Key 语义：
//   - 无 key：直接执行；
//   - 有 key：同 key + 同 body SHA-256 且 60s 内 → 重放缓存响应
//     （带 Idempotency-Replayed: true 头）；同 key 不同 body → 409；
//   - 5xx 不缓存（允许重试真实执行）。
//
// 缓存键按用户隔离（user_id + key），防止跨用户重放他人响应。
func (h *Handler) idempotency(c *gin.Context) {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		c.Next()
		return
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unable to read request body"})
		c.Abort()
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	sum := sha256.Sum256(raw)
	bodyHash := hex.EncodeToString(sum[:])

	cacheKey := userID(c).String() + "\x00" + key
	if entry, hit, hashMatch := h.idem.get(cacheKey, bodyHash); hit {
		if !hashMatch {
			c.JSON(http.StatusConflict, gin.H{"error": "idempotency key already used with different request body"})
			c.Abort()
			return
		}
		c.Header("Idempotency-Replayed", "true")
		c.Data(entry.status, "application/json; charset=utf-8", entry.body)
		c.Abort()
		return
	}
	w := &replayWriter{ResponseWriter: c.Writer}
	c.Writer = w
	c.Next()
	if w.Status() < 500 {
		h.idem.put(cacheKey, bodyHash, w.Status(), w.buf.Bytes())
	}
}

// parseBatchIDs 解析并去重 file_ids（保持顺序）：空/超上限/非 UUID 均整体 400。
// 上限经 batch.max_items 热读取（settings 未注入或读取失败回退 100）。
func (h *Handler) parseBatchIDs(c *gin.Context, raw []string) ([]uuid.UUID, bool) {
	if len(raw) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file_ids is required"})
		return nil, false
	}
	limit := h.batchLimit()
	if len(raw) > limit {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("too many items (max %d)", limit)})
		return nil, false
	}
	seen := map[uuid.UUID]bool{}
	ids := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, true
}

type batchMoveRequest struct {
	FileIDs        []string `json:"file_ids"`
	TargetParentID string   `json:"target_parent_id"`
}

// batchMove POST /api/v1/files/batch/move {file_ids[], target_parent_id?}：
// target_parent_id 缺省为个人根目录。目标目录整体校验失败（404/403）时
// 整个请求失败；其余逐项执行、部分成功——响应 per-item results
// [{id,ok,error_code}]，单项失败不回滚其他项，名称冲突项跳过。
func (h *Handler) batchMove(c *gin.Context) {
	var req batchMoveRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	ids, ok := h.parseBatchIDs(c, req.FileIDs)
	if !ok {
		return
	}
	owner := userID(c)
	var target uuid.UUID
	if strings.TrimSpace(req.TargetParentID) == "" {
		root, err := h.files.EnsureRoot(owner)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to ensure root folder"})
			return
		}
		target = root.ID
	} else if target, ok = parseID(c, req.TargetParentID); !ok {
		return
	}
	results, err := h.files.BatchMove(owner, ids, target)
	if h.fileError(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": batchResultsJSON(results)})
}

type batchIDsRequest struct {
	FileIDs []string `json:"file_ids"`
}

// batchTrash POST /api/v1/files/batch/trash {file_ids[]}：批量软删除
// （移入回收站）；根目录跳过（ROOT），团队文件要求成员写权限。
func (h *Handler) batchTrash(c *gin.Context) {
	var req batchIDsRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	ids, ok := h.parseBatchIDs(c, req.FileIDs)
	if !ok {
		return
	}
	results := h.files.BatchTrash(userID(c), ids)
	c.JSON(http.StatusOK, gin.H{"results": batchResultsJSON(results)})
}

// batchRestore POST /api/v1/files/batch/restore {file_ids[]}：批量恢复，
// 逐项复用单文件 Restore 语义（PARENT_DELETED/NAME_CONFLICT/NOT_DELETED
// 记入 per-item 结果，不静默改名）。
func (h *Handler) batchRestore(c *gin.Context) {
	var req batchIDsRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	ids, ok := h.parseBatchIDs(c, req.FileIDs)
	if !ok {
		return
	}
	results := h.files.BatchRestore(userID(c), ids)
	c.JSON(http.StatusOK, gin.H{"results": batchResultsJSON(results)})
}

func (h *Handler) batchDownload(c *gin.Context) {
	var req batchIDsRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	ids, ok := h.parseBatchIDs(c, req.FileIDs)
	if !ok {
		return
	}
	// 目录与文件混合打包：文件直入归档（条目名 <id>-<name>，与既有口径
	// 一致）；目录走 download.zip 的子树预遍历（条目数/总大小限额同目录
	// 打包下载），使批量下载与单项「下载为 ZIP」能力对齐。
	actor := userID(c)
	entries := make([]zipEntry, 0, len(ids))
	var total int64
	for _, id := range ids {
		f, err := h.files.Get(actor, id)
		if err != nil {
			continue
		}
		if f.Type == "folder" {
			if h.zipper == nil {
				continue
			}
			sub, werr := collectZipEntries(h.zipper, actor, f)
			if werr != nil {
				if h.zipWalkError(c, werr) {
					return
				}
				continue
			}
			for _, e := range sub {
				if len(entries)+1 > zipDownloadMaxEntries {
					h.zipWalkError(c, errZipDownloadLimit)
					return
				}
				total += e.size
				if total > zipDownloadMaxTotalSize {
					h.zipWalkError(c, errZipDownloadLimit)
					return
				}
				entries = append(entries, e)
			}
			continue
		}
		_, blob, err := h.files.CurrentVersion(actor, id)
		if err != nil || blob.Status != files.BlobStatusAvailable {
			continue
		}
		name := filepath.Base(f.Name)
		if name == "." || name == "\\" || name == "" {
			name = id.String()
		}
		total += blob.Size
		entries = append(entries, zipEntry{name: id.String() + "-" + name, key: blob.StorageKey, size: blob.Size})
	}
	if h.zipper == nil {
		// 无 zipper（理论上不发生，NewHandler 恒注入）：退回内联流式写出。
		c.Header("Content-Type", "application/zip")
		c.Header("Content-Disposition", `attachment; filename="docflow-files.zip"`)
		zw := zip.NewWriter(c.Writer)
		for _, e := range entries {
			w, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Deflate})
			if err != nil {
				break
			}
			reader, err := upload.ReadSection(h.storage, e.key, 0, e.size)
			if err != nil {
				continue
			}
			_, _ = io.Copy(w, reader)
			_ = reader.Close()
		}
		_ = zw.Close()
		return
	}
	h.writeZipStream(c, "docflow-files", entries)
}

func batchResultsJSON(results []files.BatchItemResult) []gin.H {
	out := make([]gin.H, 0, len(results))
	for _, r := range results {
		item := gin.H{"id": r.ID, "ok": r.OK}
		if !r.OK {
			item["error_code"] = r.ErrorCode
		}
		out = append(out, item)
	}
	return out
}
