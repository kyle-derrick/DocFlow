package share

import (
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

// ErrTreeUnavailable 表示目录分享树能力未接线（SetTreeSource 未注入），
// HTTP 层映射 503。
var ErrTreeUnavailable = errors.New("share tree is not available")

// TreeSource 抽象目录分享树所需的子树解析与清单（生产实现 *files.Store）：
// ResolveSubpath/ListFolderChildren 不做用户鉴权（匿名访问的授权由分享
// 有效性 + 子树约束保证），CurrentVersion 以分享 owner 身份读取；
// CurrentBlobs 为批量口径（目录清单 size/mime 聚合，消除逐文件 N+1）。
type TreeSource interface {
	ResolveSubpath(base files.File, path string) (files.File, []files.File, error)
	ListFolderChildren(folderID uuid.UUID, limit int) ([]files.File, error)
	CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
	CurrentBlobs(ids []uuid.UUID) map[uuid.UUID]files.ObjectBlob
}

var _ TreeSource = (*files.Store)(nil)

// TreeEntry 目录清单条目（Path 为相对分享根的 canonical 路径；不含内部
// ID 与存储信息）。
type TreeEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
}

// TreeResult 目录分享树/子路径解析结果：命中目录时 Entries 为其直接子项
// 清单（可为空切片）；命中文件时 File/Blob 非空（Blob 仅在当前版本 available
// 时填充，否则为 nil——raw 服务据此拒绝）。
type TreeResult struct {
	Share   Share
	Folder  bool
	Path    string      // 相对分享根的 canonical 路径（根目录为 ""，根文件为文件名）
	Entries []TreeEntry // Folder=true 时有效
	File    *files.File
	Blob    *files.ObjectBlob
}

// treeEntryLimit 单次目录清单的最大条目数。
const treeEntryLimit = 200

// SetTreeSource 注入目录分享树源（幂等）；未注入时 ResolveTree 返回
// ErrTreeUnavailable（HTTP 503）。生产恒注入 *files.Store。
func (s *Service) SetTreeSource(src TreeSource) {
	if src != nil {
		s.tree = src
	}
}

// ResolveTree 解析公开分享的子路径（目录清单或文件元数据）：
//   - 分享须有效（未撤销/未过期/未达上限）：否则 ErrGone（token 未知 ErrNotFound）；
//   - 根须存在且未删除（软删后分享视为失效 ErrGone）；
//   - path 在根子树内逐段解析（段校验/环检测同 files.SplitPathSegments），
//     断链/越根/不存在统一 ErrNotFound（不泄露子树细节）；
//   - 命中目录返回直接子项（子文件附当前版本 size/mime，读取失败按 0/空处理）；
//   - 命中文件返回元数据，Blob 仅在 available 时填充。
//
// 密码会话校验（HTTP 层 shareSessionAllowed）由调用方在调用前完成。
func (s *Service) ResolveTree(token, path string) (TreeResult, error) {
	sh, err := s.Resolve(token)
	if err != nil {
		return TreeResult{}, err
	}
	if s.tree == nil {
		return TreeResult{}, ErrTreeUnavailable
	}
	// 多文件打包分享（is_bundle）：根清单与子路径均以 share_files 条目为界，
	// 锚点目录的其余子项不暴露（见 resolveBundleTree）。
	if sh.Share.IsBundle {
		return s.resolveBundleTree(sh.Share, path)
	}
	root := sh.File
	if path == "" {
		if root.Type == "folder" {
			return s.folderResult(sh.Share, root, "")
		}
		return TreeResult{Share: sh.Share, Path: root.Name, File: &root, Blob: s.availableBlob(sh.Share.OwnerID, root.ID)}, nil
	}
	f, chain, err := s.tree.ResolveSubpath(root, path)
	if err != nil {
		switch {
		case errors.Is(err, files.ErrInvalidTarget),
			errors.Is(err, files.ErrInvalidName),
			errors.Is(err, files.ErrFolderDepth),
			// 子树内不存在/断链/穿越段：统一 share.ErrNotFound，不泄露细节。
			errors.Is(err, files.ErrNotFound):
			// 富文本引用资源兜底（refs，见 AttachReferenceFiles）：单文件分享的
			// 引用不在分享根子树内（如 assets/ 目录与文档同级），按路径末段
			// 文件名在 share_files 条目中匹配。仅文件条目可命中。
			if rf, rerr := s.resolveRefByName(sh.Share, path); rerr == nil {
				return rf, nil
			}
			return TreeResult{}, ErrNotFound
		}
		return TreeResult{}, err
	}
	rel := relativePath(chain)
	if f.Type == "folder" {
		return s.folderResult(sh.Share, f, rel)
	}
	return TreeResult{Share: sh.Share, Path: rel, File: &f, Blob: s.availableBlob(sh.Share.OwnerID, f.ID)}, nil
}

// resolveBundleTree 打包分享的树解析：path 为空返回打包条目清单（folder
// 语义）；非空时首段必须命中某个打包条目名——文件条目仅接受精确命中，
// 目录条目余下路径在其子树内解析（子树约束保证不越界）。未命中统一
// ErrNotFound（不泄露锚点目录其余子项的存在性）。
func (s *Service) resolveBundleTree(sh Share, path string) (TreeResult, error) {
	items, err := s.BundleItems(sh)
	if err != nil {
		return TreeResult{}, err
	}
	if path == "" {
		out := TreeResult{Share: sh, Folder: true, Path: "", Entries: make([]TreeEntry, 0, len(items))}
		blobs := s.tree.CurrentBlobs(bundleFileIDs(items))
		for _, it := range items {
			entry := TreeEntry{Name: it.Name, Type: it.Type, Path: it.Name}
			if b, ok := blobs[it.ID]; ok {
				entry.Size, entry.MimeType = b.Size, b.MimeType
			}
			out.Entries = append(out.Entries, entry)
		}
		return out, nil
	}
	first, rest, _ := strings.Cut(path, "/")
	for _, it := range items {
		if it.Name != first {
			continue
		}
		if it.Type == "file" {
			if rest != "" {
				return TreeResult{}, ErrNotFound
			}
			blob := s.availableBlob(sh.OwnerID, it.ID)
			return TreeResult{Share: sh, Path: it.Name, File: &it, Blob: blob}, nil
		}
		if rest == "" {
			return s.folderResult(sh, it, it.Name)
		}
		f, chain, rerr := s.tree.ResolveSubpath(it, rest)
		if rerr != nil {
			switch {
			case errors.Is(rerr, files.ErrInvalidTarget),
				errors.Is(rerr, files.ErrInvalidName),
				errors.Is(rerr, files.ErrFolderDepth),
				errors.Is(rerr, files.ErrNotFound):
				return TreeResult{}, ErrNotFound
			}
			return TreeResult{}, rerr
		}
		rel := it.Name + "/" + relativePath(chain)
		if f.Type == "folder" {
			return s.folderResult(sh, f, rel)
		}
		return TreeResult{Share: sh, Path: rel, File: &f, Blob: s.availableBlob(sh.OwnerID, f.ID)}, nil
	}
	return TreeResult{}, ErrNotFound
}

// resolveRefByName 引用资源兜底解析：path 末段文件名与 share_files 条目
//（refs；打包分享条目为锚点子树解析所覆盖，不会走到这里）按名匹配，命中
// 返回该文件条目（Path 保持请求的相对路径，前端 raw URL 拼接不受影响）。
// 多条目同名取首个；目录条目与已删除/不可读条目跳过（BundleItems 已过滤）。
func (s *Service) resolveRefByName(sh Share, path string) (TreeResult, error) {
	if s.tree == nil {
		return TreeResult{}, ErrTreeUnavailable
	}
	segments := strings.Split(path, "/")
	base := segments[len(segments)-1]
	if base == "" {
		return TreeResult{}, ErrNotFound
	}
	items, err := s.BundleItems(sh)
	if err != nil {
		return TreeResult{}, err
	}
	for _, it := range items {
		if it.Type != "file" || it.Name != base {
			continue
		}
		blob := s.availableBlob(sh.OwnerID, it.ID)
		if blob == nil {
			continue
		}
		f := it
		return TreeResult{Share: sh, Path: path, File: &f, Blob: blob}, nil
	}
	return TreeResult{}, ErrNotFound
}

// folderResult 构造目录命中结果（baseRel 为目录相对分享根的路径，子项
// Path 以其为前缀）。
func (s *Service) folderResult(sh Share, folder files.File, baseRel string) (TreeResult, error) {
	entries, err := s.tree.ListFolderChildren(folder.ID, treeEntryLimit)
	if err != nil {
		return TreeResult{}, err
	}
	// 子文件 size/mime 批量取当前版本 blob（单条 JOIN，原逐文件
	// CurrentVersion 为 N+1：每文件 Get+version+blob 3 次查询）。
	blobs := s.tree.CurrentBlobs(childFileIDs(entries))
	out := TreeResult{Share: sh, Folder: true, Path: baseRel, Entries: make([]TreeEntry, 0, len(entries)), File: &folder}
	for _, child := range entries {
		entry := TreeEntry{Name: child.Name, Type: child.Type, Path: child.Name}
		if baseRel != "" {
			entry.Path = baseRel + "/" + child.Name
		}
		if b, ok := blobs[child.ID]; ok {
			entry.Size, entry.MimeType = b.Size, b.MimeType
		}
		out.Entries = append(out.Entries, entry)
	}
	return out, nil
}

// childFileIDs 提取目录清单中的文件条目 ID（folder 类型不查版本）。
func childFileIDs(entries []files.File) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(entries))
	for _, e := range entries {
		if e.Type == "file" {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// bundleFileIDs 提取打包分享条目中的文件 ID。
func bundleFileIDs(items []files.File) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		if it.Type == "file" {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// relativePath 由解析链（chain[0]=分享根）构造相对路径。
func relativePath(chain []files.File) string {
	if len(chain) < 2 {
		return ""
	}
	names := make([]string, 0, len(chain)-1)
	for _, f := range chain[1:] {
		names = append(names, f.Name)
	}
	return strings.Join(names, "/")
}

// availableBlob 返回文件当前版本 blob（仅 available 时；否则 nil）。
func (s *Service) availableBlob(owner, fileID uuid.UUID) *files.ObjectBlob {
	_, blob, err := s.tree.CurrentVersion(owner, fileID)
	if err != nil || blob.Status != files.BlobStatusAvailable {
		return nil
	}
	return &blob
}
