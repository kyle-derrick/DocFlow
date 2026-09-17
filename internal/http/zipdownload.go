package http

import (
	"archive/zip"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/share"
)

// 目录打包下载（流式 zip）的安全限制：条目数 2000 / 总大小 2GB，超限 413。
const (
	zipDownloadMaxEntries   = 2000
	zipDownloadMaxTotalSize = 2 << 30
	// zipChildrenPageSize 为单目录子项列举的分页大小（ListFolderChildren
	// 上限 1000；总条目数仍由 zipDownloadMaxEntries 兜底）。
	zipChildrenPageSize = 1000
)

// errZipDownloadLimit 表示子树规模超限（条目数或总大小）。
var errZipDownloadLimit = errors.New("folder exceeds zip download limit")

// zipDownloadAPI 为打包下载所需的最小文件依赖（*files.Store 满足；接口化
// 便于单测注入内存实现）。Get 做读授权（authorizeFileRead 语义）；
// ListFolderChildren 不鉴权（认证侧已对根目录授权、公开分享侧由分享有效性
// + 子树约束保证）；CurrentVersion 的 owner 形参为「读取主体」——认证侧为
// 当前用户，公开分享侧为分享 owner。
type zipDownloadAPI interface {
	Get(user, id uuid.UUID) (files.File, error)
	ListFolderChildren(folderID uuid.UUID, limit int) ([]files.File, error)
	CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
}

var _ zipDownloadAPI = (*files.Store)(nil)

// zipEntry 为待写入归档的条目：目录名含尾斜杠；文件携带存储 key。
type zipEntry struct {
	name string
	dir  bool
	key  string
}

// downloadFolderZip GET /api/v1/files/:id/download.zip（id 为目录）：流式打包
// 子树为 zip。zip.Writer 直接包裹 HTTP 响应流（不落盘、不缓存），文件内容
// 从对象存储整对象流式拷贝；entry 名为相对根目录的路径（目录含尾斜杠），
// soft-delete 项与当前版本非 available 的文件排除。子树规模（条目数/总大小）
// 超限返回 413（预遍历在写出响应体之前完成）。权限 authorizeFileRead
// （files.Get 统一判定：个人 owner、团队在册成员/ACL）。
func (h *Handler) downloadFolderZip(c *gin.Context) {
	if h.zipper == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "zip download is not configured"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	f, err := h.zipper.Get(actor, id)
	if h.fileError(c, err) {
		return
	}
	if f.Type != "folder" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target is not a folder"})
		return
	}
	entries, err := collectZipEntries(h.zipper, actor, f)
	if h.zipWalkError(c, err) {
		return
	}
	h.writeZipStream(c, f.Name, entries)
}

// publicShareDownloadZip GET /api/v1/public/shares/:token/download.zip：目录
// 分享根的流式 zip 打包（无认证、按 IP 限流）。密码保护分享复用既有 verify
// 会话 cookie（未通过 401 PASSWORD_REQUIRED）；撤销/过期/达下载上限 410；
// 成功（消耗额度后开始流式写出）一次 ConsumeDownload（原子消费与限额语义
// 同单文件下载）。分享根非目录或 view 权限分享分别返回 400/403。
func (h *Handler) publicShareDownloadZip(c *gin.Context) {
	if h.zipper == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "zip download is not configured"})
		return
	}
	token := c.Param("token")
	// 先做不消耗计数的解析，完成密码会话校验与根类型判定后再进入消耗路径。
	pre, err := h.shares.Resolve(token)
	if publicShareError(c, err) {
		return
	}
	if !h.shareSessionAllowed(c, token, pre.Share) {
		return
	}
	if pre.File.Type != "folder" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "share root is not a folder"})
		return
	}
	r, err := h.shares.ResolveForFolderDownload(token)
	if publicShareError(c, err) {
		return
	}
	entries, err := collectZipEntries(h.zipper, r.Share.OwnerID, r.File)
	if h.zipWalkError(c, err) {
		return
	}
	h.recordAudit(c, audit.Entry{UserID: nil, Action: audit.ActionPublicDownload, ResourceType: audit.ResourceShare, ResourceID: r.Share.ID.String(), Metadata: `{"file_id":"` + r.File.ID.String() + `","kind":"zip","entries":` + strconv.Itoa(len(entries)) + `}`})
	h.recordPublicAccessEvent(c, r, share.ActionDownload)
	h.writeZipStream(c, r.File.Name, entries)
}

// zipWalkError 映射子树预遍历失败：超限 413，其余 500。
func (h *Handler) zipWalkError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errZipDownloadLimit) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "folder exceeds zip download limit", "code": "ZIP_LIMIT_EXCEEDED"})
		return true
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to walk folder"})
	return true
}

// collectZipEntries 预遍历目录子树（DFS，目录在前、名称排序由
// ListFolderChildren 保证），收集相对根的条目清单并执行规模限制：
//   - 条目数（目录与文件均计入）上限 zipDownloadMaxEntries；
//   - 文件总大小（当前版本 blob.size 之和，仅 available 文件）上限
//     zipDownloadMaxTotalSize；超限整体失败（413，不产生半截归档）；
//   - soft-delete 由 ListFolderChildren 过滤；无当前版本/非 available 的
//     文件跳过（不阻断整包）；visited 集合防御 parent 环。
func collectZipEntries(api zipDownloadAPI, owner uuid.UUID, root files.File) ([]zipEntry, error) {
	var out []zipEntry
	var total int64
	visited := map[uuid.UUID]bool{root.ID: true}
	var walk func(folderID uuid.UUID, prefix string) error
	walk = func(folderID uuid.UUID, prefix string) error {
		children, err := api.ListFolderChildren(folderID, zipChildrenPageSize)
		if err != nil {
			return err
		}
		for _, ch := range children {
			if len(out)+1 > zipDownloadMaxEntries {
				return errZipDownloadLimit
			}
			name := prefix + ch.Name
			if ch.Type == "folder" {
				if visited[ch.ID] {
					continue
				}
				visited[ch.ID] = true
				out = append(out, zipEntry{name: name + "/", dir: true})
				if err := walk(ch.ID, name+"/"); err != nil {
					return err
				}
				continue
			}
			_, blob, verr := api.CurrentVersion(owner, ch.ID)
			if verr != nil || blob.Status != files.BlobStatusAvailable {
				continue
			}
			if blob.Size < 0 || blob.Size > zipDownloadMaxTotalSize || total+blob.Size > zipDownloadMaxTotalSize {
				return errZipDownloadLimit
			}
			total += blob.Size
			out = append(out, zipEntry{name: name, key: blob.StorageKey})
		}
		return nil
	}
	if err := walk(root.ID, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// writeZipStream 把条目清单以 zip 流式写出（zip.Writer 直接包裹响应流）：
// 头部先行（application/zip + Content-Disposition attachment
// <目录名>.zip），逐条目写入并 Flush 推送进度；文件内容从对象存储整对象
// 读取流式拷贝。写出开始后错误只能截断连接（无法再改状态码）。
func (h *Handler) writeZipStream(c *gin.Context, rootName string, entries []zipEntry) {
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", contentDisposition(rootName+".zip"))
	c.Status(http.StatusOK)
	zw := zip.NewWriter(c.Writer)
	defer zw.Close()
	for _, e := range entries {
		if e.dir {
			if _, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Deflate}); err != nil {
				return
			}
			_ = zw.Flush()
			continue
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Deflate})
		if err != nil {
			return
		}
		rc, err := h.storage.Read(e.key)
		if err != nil {
			return
		}
		if _, err := io.Copy(w, rc); err != nil {
			rc.Close()
			return
		}
		rc.Close()
		_ = zw.Flush()
	}
}
