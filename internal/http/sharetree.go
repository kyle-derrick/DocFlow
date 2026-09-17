package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/docflow/docflow/internal/contenturl"
	"github.com/docflow/docflow/internal/share"
)

// publicShareTree GET /api/v1/public/shares/:token/tree/*path：
// 目录分享的树访问入口（无认证、按 IP 限流）。path 为空（或尾斜杠）返回
// 根目录清单；命中子目录返回该目录清单（条目含相对分享根的 path 与当前
// 版本 size/mime）；命中文件返回元数据。
// 密码保护分享复用既有 verify 会话 cookie（未通过时 401 PASSWORD_REQUIRED）。
// 响应附带 raw_base（/raw/share/{token}/{grant}，purpose=raw-share、绑定
// share_id 的 10 分钟短期授权）与 grant_expires_at；子资源直接拼
// raw_base + "/" + path 获取内容（无密码分享由 tree/verify 签发 grant，
// 有密码分享须先 verify 取得会话——本端点在会话校验通过后才签发）。
func (h *Handler) publicShareTree(c *gin.Context) {
	if h.contentSigner == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "raw content is not configured"})
		return
	}
	token := c.Param("token")
	rel := strings.Trim(c.Param("path"), "/")
	// 先解析分享（含撤销/过期/根存在性判定）供密码会话校验，再解析子路径。
	pre, err := h.shares.Resolve(token)
	if publicShareError(c, err) {
		return
	}
	if !h.shareSessionAllowed(c, token, pre.Share) {
		return
	}
	result, err := h.shares.ResolveTree(token, rel)
	if publicShareError(c, err) {
		return
	}
	grant, err := h.contentSigner.Sign(contenturl.Claims{
		Purpose: contenturl.PurposeRawShare,
		ShareID: result.Share.ID.String(),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to sign raw url"})
		return
	}
	out := gin.H{
		"name":             fileNameOf(result),
		"type":             "file",
		"permission":       result.Share.Permission,
		"expires_at":       result.Share.ExpiresAt,
		"raw_base":         "/raw/share/" + token + "/" + grant,
		"grant_expires_at": h.contentSigner.ExpiresAt().Format(time.RFC3339),
	}
	if result.Folder {
		out["type"] = "folder"
		out["path"] = result.Path
		out["entries"] = result.Entries
		c.JSON(http.StatusOK, out)
		return
	}
	out["path"] = result.Path
	if result.File != nil {
		out["size"] = int64(0)
		out["mime_type"] = ""
		if result.Blob != nil {
			out["size"] = result.Blob.Size
			out["mime_type"] = result.Blob.MimeType
		}
	}
	c.JSON(http.StatusOK, out)
}

func fileNameOf(r share.TreeResult) string {
	if r.File != nil {
		return r.File.Name
	}
	return ""
}

// serveRawShare GET/HEAD /raw/share/:token/:grant/*path：目录分享子资源的
// 受控原始内容。校验顺序：grant（purpose=raw-share、绑定 share_id）→
// 分享有效（撤销/过期/达上限 410，未知 404）→ grant 与 token 归属一致 →
// 根存在且路径在子树内 → 文件当前版本 blob available → 扩展名白名单/
// Range/安全头（与 /raw/auth 同一套 serveRawBlob）。目录无尾斜杠 308、
// 尾斜杠解析 index.html。子资源 raw 不消耗分享 download_count（显式下载
// 仍走既有 download 端点的 ConsumeDownload）。
func (h *Handler) serveRawShare(c *gin.Context) {
	if h.contentSigner == nil {
		rawNotFound(c)
		return
	}
	claims, err := h.contentSigner.VerifyPurpose(c.Param("grant"), contenturl.PurposeRawShare)
	if err != nil {
		rawNotFound(c)
		return
	}
	token := c.Param("token")
	// 分享有效性（410/404 语义）与 grant 归属一致性。
	r, err := h.shares.Resolve(token)
	if err != nil {
		publicShareError(c, err)
		return
	}
	if r.Share.ID.String() != claims.ShareID {
		rawNotFound(c)
		return
	}
	rawPathParam := c.Param("path")
	result, err := h.shares.ResolveTree(token, strings.Trim(rawPathParam, "/"))
	if err != nil {
		publicShareError(c, err)
		return
	}
	if result.Folder {
		if !strings.HasSuffix(rawPathParam, "/") {
			h.rawRedirectTrailingSlash(c)
			return
		}
		rel := strings.Trim(rawPathParam, "/")
		indexRel := "index.html"
		if rel != "" {
			indexRel = rel + "/index.html"
		}
		index, ierr := h.shares.ResolveTree(token, indexRel)
		if ierr != nil || index.Folder || index.File == nil || index.Blob == nil {
			rawNotFound(c)
			return
		}
		result = index
	}
	if result.File == nil || result.Blob == nil {
		rawNotFound(c)
		return
	}
	h.serveRawBlob(c, result.File.Name, *result.Blob)
}
