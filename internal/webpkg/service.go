package webpkg

import (
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

// FileSource 抽象网页包服务对文件元数据的访问（*files.Store 满足）。
// GetFileByID/CurrentVersion 的权限由调用方保证：自动解包为系统行为
// （上传完成钩子），手动解包在 HTTP 层先做 CanWrite 鉴权。
type FileSource interface {
	GetFileByID(id uuid.UUID) (files.File, error)
	CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
}

var _ FileSource = (*files.Store)(nil)

// Service 提供网页包的自动/手动解包与内容解析。
type Service struct {
	repo    Repo
	files   FileSource
	storage Storage
	limits  Limits
	// enabled 控制 AutoExtract（WEBPKG_ENABLED）；手动解包不受其限制。
	enabled bool
}

func NewService(repo Repo, source FileSource, storage Storage, limits Limits) *Service {
	return &Service{repo: repo, files: source, storage: storage, limits: limits.withDefaults(), enabled: true}
}

// SetEnabled 控制自动解包开关（幂等）。
func (s *Service) SetEnabled(v bool) { s.enabled = v }

// AutoExtract 上传完成钩子的自动入口：仅对 zip 候选文件尝试解包，
// 失败置 blocked/failed 不影响文件本身可用性；任何错误仅记日志。
func (s *Service) AutoExtract(fileID uuid.UUID) {
	if s == nil || !s.enabled || s.repo == nil || s.files == nil || s.storage == nil {
		return
	}
	f, err := s.files.GetFileByID(fileID)
	if err != nil {
		return // 文件不存在/已删除：无需解包
	}
	_, blob, err := s.files.CurrentVersion(f.OwnerID, fileID)
	if err != nil || !ZipCandidate(blob.MimeType, f.Name) {
		return
	}
	if _, err := s.ExtractForFile(fileID); err != nil {
		log.Printf("[webpkg] auto extract file %s: %v", fileID, err)
	}
}

// ExtractForFile 对文件当前版本执行解包（手动端点与自动钩子共用）。
// 幂等重建：web_packages 行与 public_id 首次创建后保持稳定，重跑先按
// 内部清单删除旧 key 再解包。返回最终状态的行；解包失败时也返回行
// （status=blocked/failed，附原因），便于调用方展示。
func (s *Service) ExtractForFile(fileID uuid.UUID) (Package, error) {
	f, err := s.files.GetFileByID(fileID)
	if err != nil {
		return Package{}, err
	}
	_, blob, err := s.files.CurrentVersion(f.OwnerID, fileID)
	if err != nil {
		return Package{}, err
	}
	if blob.Status != files.BlobStatusAvailable {
		return Package{}, ErrBlobUnavailable
	}

	pkg, err := s.repo.GetByFileID(fileID)
	switch {
	case errors.Is(err, ErrNotFound):
		pid, perr := NewPublicID()
		if perr != nil {
			return Package{}, perr
		}
		pkg = Package{ID: uuid.New(), FileID: fileID, PublicID: pid, Status: StatusExtracting}
		if cerr := s.repo.Create(pkg); cerr != nil {
			return Package{}, cerr
		}
	case err != nil:
		return Package{}, err
	default:
		_ = s.repo.SetResult(pkg.ID, StatusExtracting, "", 0, 0)
		pkg.Status, pkg.Error, pkg.EntryCount, pkg.TotalSize = StatusExtracting, "", 0, 0
	}

	prefix := "webpkg/" + pkg.PublicID
	// 幂等重建：先删旧 key（清单驱动），失败不阻断（旧对象由覆盖写与清单兜底）。
	Remove(s.storage, prefix)

	r, rerr := s.storage.Read(blob.StorageKey)
	if rerr != nil {
		_ = s.repo.SetResult(pkg.ID, StatusFailed, rerr.Error(), 0, 0)
		pkg.Status, pkg.Error = StatusFailed, rerr.Error()
		return pkg, rerr
	}
	stats, xerr := Extract(s.storage, r, prefix, s.limits)
	r.Close()
	if xerr != nil {
		status := StatusFailed
		if errors.Is(xerr, ErrWebpkgInvalid) {
			status = StatusBlocked
		}
		_ = s.repo.SetResult(pkg.ID, status, xerr.Error(), 0, 0)
		pkg.Status, pkg.Error = status, xerr.Error()
		return pkg, xerr
	}
	if serr := s.repo.SetResult(pkg.ID, StatusReady, "", stats.EntryCount, stats.TotalSize); serr != nil {
		return pkg, serr
	}
	pkg.Status, pkg.Error, pkg.EntryCount, pkg.TotalSize = StatusReady, "", stats.EntryCount, stats.TotalSize
	return pkg, nil
}

// ReadyPackage 返回文件 ready 包的 public_id（预览联动入口）。
func (s *Service) ReadyPackage(fileID uuid.UUID) (string, bool) {
	if s == nil || s.repo == nil || s.files == nil {
		return "", false
	}
	pkg, err := s.repo.GetByFileID(fileID)
	if err != nil || pkg.Status != StatusReady {
		return "", false
	}
	return pkg.PublicID, true
}

// Resolve 按 publicId + 相对路径取内容 reader 与推断的 Content-Type。
// 校验（任一失败返回 ok=false，HTTP 侧统一 404 不泄露细节）：
// public_id 形如 43 字符 URL-safe 串、包 ready、文件未删除、当前版本
// blob available、相对路径通过清理与扩展名白名单、内容键存在。
// svg 返回 image/svg+xml，由 HTTP 层以最严格 CSP（sandbox）提供。
func (s *Service) Resolve(publicID, relPath string) (io.ReadCloser, string, bool) {
	if s == nil || s.repo == nil || s.files == nil || s.storage == nil {
		return nil, "", false
	}
	if !URLSafePublicID(publicID) {
		return nil, "", false
	}
	pkg, err := s.repo.GetByPublicID(publicID)
	if err != nil || pkg.Status != StatusReady {
		return nil, "", false
	}
	f, err := s.files.GetFileByID(pkg.FileID)
	if err != nil || f.DeletedAt != nil {
		return nil, "", false
	}
	_, blob, err := s.files.CurrentVersion(f.OwnerID, pkg.FileID)
	if err != nil || blob.Status != files.BlobStatusAvailable {
		return nil, "", false
	}
	clean, contentType, ok := ResolvePath(relPath, s.limits.MaxDepth)
	if !ok {
		return nil, "", false
	}
	rc, err := s.storage.Read(fmt.Sprintf("webpkg/%s/%s", pkg.PublicID, clean))
	if err != nil {
		return nil, "", false
	}
	return rc, contentType, true
}
