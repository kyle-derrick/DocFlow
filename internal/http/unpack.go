package http

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/webpkg"
)

// zip 解包导入的安全限制（计划约定：条目数 500 / 展开总量 500MB）。
const (
	unpackMaxEntries   = 500
	unpackMaxTotalSize = 500 << 20
	// unpackMaxDepth 条目路径清洗的目录深度上限（与 folder.max_depth 默认 32
	// 对齐；实际落库仍由 CreateFolderIn 的深度校验兜底）。
	unpackMaxDepth = 32
)

// unpackFailure 为失败条目明细（聚合并原样返回，不中断其余条目）。
type unpackFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// unpackResult 为 POST /files/:id/unpack 的响应。
type unpackResult struct {
	RootFolderID   uuid.UUID       `json:"root_folder_id"`
	CreatedFiles   int             `json:"created_files"`
	CreatedFolders int             `json:"created_folders"`
	Skipped        int             `json:"skipped"`
	Failures       []unpackFailure `json:"failures"`
}

// unpackAPI 是 zip 解包所需的最小文件依赖（*files.Store 满足；接口化便于
// 单测注入内存实现）。
type unpackAPI interface {
	Get(user, id uuid.UUID) (files.File, error)
	ValidateFolder(user, parent uuid.UUID) error
	CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
	FindChildByName(parent uuid.UUID, name string) (files.File, error)
	CreateFolderIn(user, parent uuid.UUID, name string) (files.File, error)
}

var _ unpackAPI = (*files.Store)(nil)

// unpackZip POST /api/v1/files/:id/unpack：把 zip 文件（id）解为真实文件树。
// 在 zip 所在父目录下以 zip 名（去扩展）建目录（已存在则复用，幂等），
// 逐条目经 upload 管线落库（UploadBytes：office 校验/扩展名黑名单/扫描/
// 配额自然生效，MIME 按扩展映射）。路径清洗复用 webpkg.SanitizePath
// （Zip Slip/控制字符/保留名/深度），条目数与展开总量超限整体拒绝（400，
// 不产生半提交）；单条目失败聚合进 failures（同名条目按已存在跳过，幂等）。
func (h *Handler) unpackZip(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	f, err := h.unpacker.Get(actor, id)
	if h.fileError(c, err) {
		return
	}
	if f.Type != "file" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target must be a zip file"})
		return
	}
	if f.ParentID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "zip file has no parent folder"})
		return
	}
	// 写授权：目标父目录须可写（个人 owner / 团队 CanWrite+ACL 复用）。
	if err := h.unpacker.ValidateFolder(actor, *f.ParentID); err != nil {
		h.fileError(c, err)
		return
	}
	_, blob, err := h.unpacker.CurrentVersion(actor, id)
	if err != nil {
		h.fileError(c, err)
		return
	}
	if blob.Status != files.BlobStatusAvailable {
		c.JSON(http.StatusForbidden, gin.H{"error": "file is not available", "status": blob.Status})
		return
	}
	entries, closer, err := readZipEntries(h, blob)
	if err != nil {
		unpackError(c, err)
		return
	}
	defer closer()

	// 预校验（全量通过才动库，避免结构类失败半提交）。
	plan, err := planUnpack(entries)
	if err != nil {
		unpackError(c, err)
		return
	}
	// 目标目录：zip 名去扩展，已存在（目录）则复用（幂等）。
	destName := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
	if destName == "" {
		destName = f.Name
	}
	result := unpackResult{}
	root, err := h.unpackEnsureDir(actor, *f.ParentID, []string{destName}, &result.CreatedFolders)
	if err != nil {
		h.fileError(c, err)
		return
	}
	result.RootFolderID = root.ID
	// 目录路径缓存（clean 相对路径 → 目录行），按路径排序保证父先于子。
	folderCache := map[string]files.File{}
	var totalRead int64
	for _, item := range plan {
		segs := strings.Split(item.clean, "/")
		dirSegs, name := segs[:len(segs)-1], segs[len(segs)-1]
		dir := root
		if len(dirSegs) > 0 {
			if cached, ok := folderCache[item.clean[:strings.LastIndex(item.clean, "/")]]; ok {
				dir = cached
			} else {
				dir, err = h.unpackEnsureDir(actor, root.ID, dirSegs, &result.CreatedFolders)
				if err != nil {
					result.Failures = append(result.Failures, unpackFailure{Path: item.clean, Error: err.Error()})
					continue
				}
				folderCache[item.clean[:strings.LastIndex(item.clean, "/")]] = dir
			}
		}
		data, rerr := readZipEntry(item.fh, unpackMaxTotalSize-totalRead)
		if rerr != nil {
			result.Failures = append(result.Failures, unpackFailure{Path: item.clean, Error: rerr.Error()})
			continue
		}
		totalRead += int64(len(data))
		if _, uerr := h.uploads.UploadBytes(actor, dir.ID, name, data); uerr != nil {
			if errors.Is(uerr, files.ErrConflict) {
				result.Skipped++ // 同名已存在：幂等跳过
				continue
			}
			result.Failures = append(result.Failures, unpackFailure{Path: item.clean, Error: uerr.Error()})
			continue
		}
		result.CreatedFiles++
	}
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: "file.unpack", ResourceType: audit.ResourceFile, ResourceID: f.ID.String(), Metadata: fmt.Sprintf(`{"root_folder_id":"%s","created_files":%d,"created_folders":%d,"skipped":%d,"failures":%d}`, root.ID, result.CreatedFiles, result.CreatedFolders, result.Skipped, len(result.Failures))})
	c.JSON(http.StatusOK, result)
}

// unpackEntry 为预校验通过的待写入条目。
type unpackEntry struct {
	fh    *zip.File
	clean string
}

// readZipEntries 把 blob spool 到临时文件并打开 zip 归档（zip.NewReader
// 需要 ReaderAt；条目内容懒加载，返回的 closer 释放暂存）。
func readZipEntries(h *Handler, blob files.ObjectBlob) ([]*zip.File, func(), error) {
	rc, err := h.storage.Read(blob.StorageKey)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to read zip: %w", err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp("", "docflow-unpack-*.zip")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	n, err := io.Copy(tmp, io.LimitReader(rc, blob.Size+1))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if n != blob.Size {
		cleanup()
		return nil, nil, errors.New("zip content size mismatch")
	}
	zr, err := zip.NewReader(tmp, n)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("%w: not a valid zip archive", webpkg.ErrWebpkgInvalid)
	}
	return zr.File, cleanup, nil
}

// planUnpack 预校验全部条目并按路径排序：路径清洗（webpkg.SanitizePath，
// Zip Slip/控制字符/保留名/深度）、条目数、展开总量与重名；空 zip 拒绝。
// 校验全量通过才返回计划（避免结构类失败半提交）。
func planUnpack(entries []*zip.File) ([]unpackEntry, error) {
	if len(entries) > unpackMaxEntries {
		return nil, fmt.Errorf("%w: %d entries exceed limit %d", webpkg.ErrWebpkgInvalid, len(entries), unpackMaxEntries)
	}
	seen := map[string]bool{}
	var plan []unpackEntry
	var declared uint64
	for _, fh := range entries {
		if fh.FileInfo().IsDir() || strings.HasSuffix(fh.Name, "/") {
			continue // 目录条目隐式创建
		}
		clean, isDir, err := webpkg.SanitizePath(fh.Name, unpackMaxDepth)
		if err != nil || isDir || clean == "" {
			return nil, fmt.Errorf("%w: invalid entry %q", webpkg.ErrWebpkgInvalid, fh.Name)
		}
		if seen[clean] {
			return nil, fmt.Errorf("%w: duplicate entry %q", webpkg.ErrWebpkgInvalid, clean)
		}
		seen[clean] = true
		declared += fh.UncompressedSize64
		if declared > unpackMaxTotalSize {
			return nil, fmt.Errorf("%w: declared total size exceeds limit %d", webpkg.ErrWebpkgInvalid, unpackMaxTotalSize)
		}
		plan = append(plan, unpackEntry{fh: fh, clean: clean})
	}
	if len(plan) == 0 {
		return nil, fmt.Errorf("%w: archive contains no files", webpkg.ErrWebpkgInvalid)
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].clean < plan[j].clean })
	return plan, nil
}

// readZipEntry 读取条目内容（实际字节数双保险：不超剩余总量额度）。
func readZipEntry(fh *zip.File, remaining int64) ([]byte, error) {
	rc, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if remaining <= 0 {
		return nil, fmt.Errorf("%w: total extracted size exceeds limit %d", webpkg.ErrWebpkgInvalid, unpackMaxTotalSize)
	}
	data, err := io.ReadAll(io.LimitReader(rc, remaining+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > remaining {
		return nil, fmt.Errorf("%w: total extracted size exceeds limit %d", webpkg.ErrWebpkgInvalid, unpackMaxTotalSize)
	}
	return data, nil
}

// unpackEnsureDir 逐段查找/创建目录（find-or-create，幂等复用同名目录；
// 同名文件挡路视为冲突）。created 非 nil 时按实际新建数递增。
func (h *Handler) unpackEnsureDir(actor uuid.UUID, root uuid.UUID, segs []string, created *int) (files.File, error) {
	parent := files.File{ID: root, Type: "folder"}
	for _, seg := range segs {
		child, cerr := h.unpacker.FindChildByName(parent.ID, seg)
		if cerr == nil {
			if child.Type != "folder" {
				return files.File{}, files.ErrConflict
			}
			parent = child
			continue
		}
		if !errors.Is(cerr, files.ErrNotFound) {
			return files.File{}, cerr
		}
		createdFolder, aerr := h.unpacker.CreateFolderIn(actor, parent.ID, seg)
		if aerr != nil {
			return files.File{}, aerr
		}
		parent = createdFolder
		if created != nil {
			*created++
		}
	}
	return parent, nil
}

// unpackError 映射解包失败：安全校验类 400（code=UNPACK_INVALID），
// 其余 500。
func unpackError(c *gin.Context, err error) {
	if errors.Is(err, webpkg.ErrWebpkgInvalid) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "UNPACK_INVALID"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to unpack zip"})
}
