package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/upload"
)

// maxReadBytes 为 df_read_file 的内容大小上限（与搜索索引的内容上限一致，
// 2MiB）：超出提示走 download 链接，避免大文件撑爆模型上下文。
const maxReadBytes = 2 << 20

// ---------- 能力可用性判定（Available） ----------

func filesAvailable(d *Deps) bool { return d.Files != nil }

func writeAvailable(d *Deps) bool { return d.Files != nil && d.Uploads != nil }

func readAvailable(d *Deps) bool { return d.Files != nil && d.Storage != nil }

func sharesAvailable(d *Deps) bool { return d.Shares != nil }

func spacesAvailable(d *Deps) bool { return d.Spaces != nil }

func searchAvailable(d *Deps) bool { return d.Search != nil }

// ---------- 工具清单 ----------

// allTools 返回全部 df_ 工具（手写 JSON Schema；名称统一 df_ 前缀）。
func allTools() []Tool {
	prop := func(desc string, extra ...map[string]any) map[string]any {
		p := map[string]any{"type": "string", "description": desc}
		for _, e := range extra {
			for k, v := range e {
				p[k] = v
			}
		}
		return p
	}
	spaceIDProp := prop("空间 ID（UUID）；缺省为你的默认空间，df_list_spaces 可列出全部可见空间")
	parentIDProp := prop("父目录 file_id；缺省为对应空间根目录")
	fileIDProp := prop("文件或目录的 file_id（UUID）")
	limitProp := func(max int) map[string]any {
		return map[string]any{"type": "integer", "minimum": 1, "maximum": max, "default": 20, "description": "返回条数上限"}
	}
	contentTextProp := prop("文本内容（UTF-8）：与 content_base64 二选一。drawio 为 XML、excalidraw 为 JSON，均可按文本直接写入")
	contentBase64Prop := prop("二进制内容的 base64 编码：与 content_text 二选一")

	return []Tool{
		{
			Name:        "df_list_files",
			Description: "列出目录内容（缺省根目录）：指定空间（space_id 缺省为默认空间），返回子项 id/name/type/parent_id 等。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"space_id": spaceIDProp, "parent_id": parentIDProp,
				"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "default": 50, "description": "返回条数上限"},
				"offset": map[string]any{"type": "integer", "minimum": 0, "default": 0, "description": "分页偏移（服务端截断实现）"},
			}},
			Available: filesAvailable,
			Handler:   toolListFiles,
		},
		{
			Name:        "df_get_file",
			Description: "获取文件/目录元数据与当前版本信息（版本号、大小、sha256、MIME、状态）。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_id": fileIDProp}, "required": []string{"file_id"}},
			Available:   filesAvailable,
			Handler:     toolGetFile,
		},
		{
			Name:        "df_create_folder",
			Description: "创建子目录（空间内；写权限按空间角色/ACL 判定）。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"name": prop("目录名（NFC 归一，拒绝非法字符/保留名，≤255 字符）"), "parent_id": parentIDProp,
				"space_id": spaceIDProp,
			}, "required": []string{"name"}},
			Available: filesAvailable,
			Handler:   toolCreateFolder,
		},
		{
			Name:        "df_write_file",
			Description: "新建文件并写入内容：走上传管线（office 格式校验、病毒扫描、空间配额、扩展名黑名单、MIME 按扩展名推断均生效）。同名冲突时换名重试。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"name": prop("文件名（含扩展名，MIME 与校验按扩展名处理）"), "parent_id": parentIDProp,
				"space_id":     spaceIDProp,
				"content_text": contentTextProp, "content_base64": contentBase64Prop,
			}, "required": []string{"name"}},
			Available: writeAvailable,
			Handler:   toolWriteFile,
		},
		{
			Name:        "df_write_version",
			Description: "覆盖既有文件为新版本（内容不可变版本链）：走覆盖上传管线（校验/扫描/配额生效），可用于编辑文本、drawio XML、excalidraw JSON 等。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"file_id": fileIDProp, "content_text": contentTextProp, "content_base64": contentBase64Prop,
			}, "required": []string{"file_id"}},
			Available: writeAvailable,
			Handler:   toolWriteVersion,
		},
		{
			Name:        "df_read_file",
			Description: "读取文件内容（当前或指定版本）：文本类（md/txt/json/xml/js/drawio/excalidraw 等）以 text 直读，二进制用 base64；超过 2MB 提示改用 download 链接。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"file_id":    fileIDProp,
				"version_id": prop("版本 ID（UUID）；缺省为当前版本"),
				"as":         prop("返回编码：text（缺省，仅文本类）/ base64", map[string]any{"enum": []string{"text", "base64"}, "default": "text"}),
			}, "required": []string{"file_id"}},
			Available: readAvailable,
			Handler:   toolReadFile,
		},
		{
			Name:        "df_rename_file",
			Description: "重命名文件或目录（根目录不可改名）。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"file_id": fileIDProp, "name": prop("新名称"),
			}, "required": []string{"file_id", "name"}},
			Available: filesAvailable,
			Handler:   toolRenameFile,
		},
		{
			Name:        "df_move_file",
			Description: "移动文件/目录到新父目录（不能移动到自身或其子孙目录下）。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"file_id": fileIDProp, "parent_id": prop("目标父目录 file_id"),
			}, "required": []string{"file_id", "parent_id"}},
			Available: filesAvailable,
			Handler:   toolMoveFile,
		},
		{
			Name:        "df_copy_file",
			Description: "复制文件到目标目录（仅文件，目录不可复制；name 缺省为「原名 copy」）。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"file_id": fileIDProp, "parent_id": prop("目标父目录 file_id"), "name": prop("新文件名（可选）"),
			}, "required": []string{"file_id", "parent_id"}},
			Available: filesAvailable,
			Handler:   toolCopyFile,
		},
		{
			Name:        "df_delete_file",
			Description: "删除文件/目录到回收站（软删除，可用 df_restore_file 恢复；根目录不可删）。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_id": fileIDProp}, "required": []string{"file_id"}},
			Available:   filesAvailable,
			Handler:     toolDeleteFile,
		},
		{
			Name:        "df_restore_file",
			Description: "从回收站恢复软删除的文件/目录（父目录仍在回收站或名称冲突时报错）。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_id": fileIDProp}, "required": []string{"file_id"}},
			Available:   filesAvailable,
			Handler:     toolRestoreFile,
		},
		{
			Name:        "df_list_trash",
			Description: "列出回收站顶层条目（指定空间；space_id 缺省为默认空间）。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"space_id": spaceIDProp, "limit": limitProp(200),
			}},
			Available: filesAvailable,
			Handler:   toolListTrash,
		},
		{
			Name:        "df_list_versions",
			Description: "列出文件全部历史版本（版本号倒序；含大小、sha256、MIME、状态）。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_id": fileIDProp}, "required": []string{"file_id"}},
			Available:   filesAvailable,
			Handler:     toolListVersions,
		},
		{
			Name:        "df_restore_version",
			Description: "把文件的 current_version 回滚到既有历史版本（版本内容不可变）。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"file_id": fileIDProp, "version_id": prop("目标版本 ID（须属于该文件）"),
			}, "required": []string{"file_id", "version_id"}},
			Available: filesAvailable,
			Handler:   toolRestoreVersion,
		},
		{
			Name:        "df_search_files",
			Description: "全文检索文件（文件名 + 文本内容；覆盖全部可见空间）。需部署启用搜索，未启用时调用返回明确错误。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"query": prop("检索关键词（名称子串匹配或内容分词命中）"), "limit": limitProp(100),
			}, "required": []string{"query"}},
			Available: searchAvailable,
			Handler:   toolSearchFiles,
		},
		{
			Name:        "df_list_shares",
			Description: "列出当前用户创建的分享（含关联文件名、权限、有效期、撤销状态）。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"limit": limitProp(200)}},
			Available:   sharesAvailable,
			Handler:     toolListShares,
		},
		{
			Name:        "df_create_share",
			Description: "为文件创建分享：public 返回一次性 token 与访问链接（可附密码）；private 授权给指定用户/空间（须再提供 user_ids/space_ids）。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"file_id":         fileIDProp,
				"permission":      prop("分享权限", map[string]any{"enum": []string{"view", "download"}, "default": "view"}),
				"visibility":      prop("可见性：public 公开链接（缺省）/ private 显式授权", map[string]any{"enum": []string{"public", "private"}, "default": "public"}),
				"expires_in_days": map[string]any{"type": "integer", "minimum": 0, "description": "有效天数；缺省/0 用部署默认或永久"},
				"password":        prop("公开分享访问密码（4-64 字符，仅 public 可用）"),
				"max_downloads":   map[string]any{"type": "integer", "minimum": 0, "description": "下载次数上限（可选）"},
				"user_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "私有分享授权用户 ID 列表"},
				"space_ids":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "私有分享授权空间 ID 列表（空间全体成员可访问）"},
			}, "required": []string{"file_id"}},
			Available: sharesAvailable,
			Handler:   toolCreateShare,
		},
		{
			Name:        "df_revoke_share",
			Description: "撤销分享（幂等；撤销后公开链接立即失效）。",
			Scope:       ScopeFilesWrite,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"share_id": prop("分享 ID（UUID）")}, "required": []string{"share_id"}},
			Available:   sharesAvailable,
			Handler:     toolRevokeShare,
		},
		{
			Name:        "df_list_spaces",
			Description: "列出当前用户可见的空间（id/name/description/owner_id/is_default），供 space_id 定位目标空间。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Available:   spacesAvailable,
			Handler:     toolListSpaces,
		},
		{
			Name:        "df_resolve_path",
			Description: "按路径定位文件/目录并返回 file_id（如 path=\"docs/报告.md\"；空间内相对路径，space_id 缺省为默认空间）。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"space_id": spaceIDProp,
				"path":     prop("以 / 分隔的相对路径（空串为根目录）"),
			}, "required": []string{"path"}},
			Available: filesAvailable,
			Handler:   toolResolvePath,
		},
		{
			Name:        "df_download_url",
			Description: "返回文件的下载/预览 API 路径（大文件超出 df_read_file 上限时，让用户或客户端带 Bearer 凭证经此链接获取）。",
			Scope:       ScopeFilesRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_id": fileIDProp}, "required": []string{"file_id"}},
			Available:   filesAvailable,
			Handler:     toolDownloadURL,
		},
	}
}

// ---------- 参数与定位辅助 ----------

// userError 为面向调用方的可读错误（errorMessage 原样透传）。
type userError struct{ message string }

func (e *userError) Error() string { return e.message }

// fail 构造可读的业务失败信息（供 agent 阅读后自行调整）。
func fail(format string, a ...any) error { return &userError{message: fmt.Sprintf(format, a...)} }

func decodeArgs(args json.RawMessage, target any) error {
	if len(args) == 0 || string(args) == "null" {
		return nil
	}
	if err := json.Unmarshal(args, target); err != nil {
		return badArgs("invalid arguments: %v", err)
	}
	return nil
}

func parseUUIDField(name, value string) (uuid.UUID, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return uuid.Nil, badArgs("%s is required", name)
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, badArgs("%s is not a valid UUID", name)
	}
	return id, nil
}

// requireSpaceRead 校验用户对空间的读权限（CanRead：在册成员），
// 语义同 http 层 requireSpaceRead；未通过时按「不存在」处理不泄露存在性。
func requireSpaceRead(deps *Deps, user, spaceID uuid.UUID) error {
	if deps.Spaces == nil {
		return errToolUnavailable
	}
	ok, err := deps.Spaces.CanRead(user, spaceID)
	if err != nil {
		return err
	}
	if !ok {
		return fail("space not found or you are not a member")
	}
	return nil
}

// resolveSpaceID 解析 space 参数：空串回退用户默认空间 ID。
func resolveSpaceID(deps *Deps, user uuid.UUID, raw string) (uuid.UUID, error) {
	if v := strings.TrimSpace(raw); v != "" {
		return parseUUIDField("space_id", v)
	}
	if deps.Spaces == nil {
		return uuid.Nil, errToolUnavailable
	}
	sp, err := deps.Spaces.DefaultSpace(user)
	if err != nil {
		return uuid.Nil, fail("default space not found; pass space_id explicitly")
	}
	return sp.ID, nil
}

// resolveParent 解析父目录（读取或写入场景共用）：space_id 缺省为用户默认
// 空间（经 Spaces 服务解析；要求空间读权限），parent_id 缺省为空间根目录。
// 写权限由下游服务（CreateFolderIn / 上传管线 ValidateFolder）判定，不旁路。
func resolveParent(deps *Deps, id Identity, spaceIDRaw, parentIDRaw string) (files.File, error) {
	user := id.UserID
	spaceID, err := resolveSpaceID(deps, user, spaceIDRaw)
	if err != nil {
		return files.File{}, err
	}
	if err := requireSpaceRead(deps, user, spaceID); err != nil {
		return files.File{}, err
	}
	if parentIDRaw == "" {
		return deps.Files.SpaceRoot(spaceID)
	}
	parentID, err := parseUUIDField("parent_id", parentIDRaw)
	if err != nil {
		return files.File{}, err
	}
	return deps.Files.GetSpaceFolder(spaceID, parentID)
}

// decodeContent 解码 content_text / content_base64 二选一字段。
func decodeContent(text, base64Content string) ([]byte, error) {
	switch {
	case text != "" && base64Content != "":
		return nil, badArgs("provide either content_text or content_base64, not both")
	case text != "":
		return []byte(text), nil
	case base64Content != "":
		data, err := base64.StdEncoding.DecodeString(base64Content)
		if err != nil {
			return nil, badArgs("content_base64 is not valid base64: %v", err)
		}
		return data, nil
	default:
		return nil, badArgs("content_text or content_base64 is required")
	}
}

// ---------- 结果序列化 ----------

func fileJSON(f files.File) map[string]any {
	return map[string]any{
		"id": f.ID, "name": f.Name, "parent_id": f.ParentID, "type": f.Type,
		"is_root": f.IsRoot, "space_id": f.SpaceID,
		"description": f.Description, "is_starred": f.IsStarred,
		"created_at": f.CreatedAt, "updated_at": f.UpdatedAt,
	}
}

func versionDetailJSON(v files.VersionDetail) map[string]any {
	return map[string]any{
		"id": v.ID, "version": v.Version, "size": v.Size, "sha256": v.SHA256,
		"mime_type": v.MimeType, "status": v.Status, "comment": v.Comment,
		"user_id": v.UserID, "created_at": v.CreatedAt,
	}
}

func currentVersionJSON(v files.FileVersion, b files.ObjectBlob) map[string]any {
	return map[string]any{
		"id": v.ID, "version": v.Version, "size": v.Size, "sha256": v.ContentSHA256,
		"mime_type": b.MimeType, "status": b.Status, "user_id": v.UserID, "created_at": v.CreatedAt,
	}
}

func filesJSONList(out []files.File) []map[string]any {
	items := make([]map[string]any, 0, len(out))
	for _, f := range out {
		items = append(items, fileJSON(f))
	}
	return items
}

// ---------- 工具实现 ----------

func toolListFiles(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID  string `json:"space_id"`
		ParentID string `json:"parent_id"`
		Limit    int    `json:"limit"`
		Offset   int    `json:"offset"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	limit, offset := a.Limit, a.Offset
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	fetch := limit + offset
	parent, err := resolveParent(deps, id, a.SpaceID, a.ParentID)
	if err != nil {
		return nil, err
	}
	out, lerr := deps.Files.ListSpace(parent.SpaceID, parent.ID, fetch, files.SpaceListFilter{})
	if lerr != nil {
		return nil, lerr
	}
	if offset > 0 {
		if offset < len(out) {
			out = out[offset:]
		} else {
			out = nil
		}
	}
	return map[string]any{"parent_id": parent.ID, "space_id": parent.SpaceID, "files": filesJSONList(out), "count": len(out)}, nil
}

func toolGetFile(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID string `json:"file_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	f, err := deps.Files.Get(id.UserID, fileID)
	if err != nil {
		return nil, err
	}
	out := fileJSON(f)
	if f.Type == "file" && f.CurrentVersionID != nil {
		if v, blob, verr := deps.Files.CurrentVersion(id.UserID, fileID); verr == nil {
			out["current_version"] = currentVersionJSON(v, blob)
		}
	}
	return out, nil
}

func toolCreateFolder(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		Name     string `json:"name"`
		ParentID string `json:"parent_id"`
		SpaceID  string `json:"space_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Name) == "" {
		return nil, badArgs("name is required")
	}
	parent, err := resolveParent(deps, id, a.SpaceID, a.ParentID)
	if err != nil {
		return nil, err
	}
	// CreateFolderIn 自带授权（空间 CanWrite+ACL）并继承空间归属。
	f, err := deps.Files.CreateFolderIn(id.UserID, parent.ID, a.Name)
	if err != nil {
		return nil, err
	}
	return fileJSON(f), nil
}

func toolWriteFile(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		Name          string `json:"name"`
		ParentID      string `json:"parent_id"`
		SpaceID       string `json:"space_id"`
		ContentText   string `json:"content_text"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Name) == "" {
		return nil, badArgs("name is required")
	}
	data, err := decodeContent(a.ContentText, a.ContentBase64)
	if err != nil {
		return nil, err
	}
	parent, err := resolveParent(deps, id, a.SpaceID, a.ParentID)
	if err != nil {
		return nil, err
	}
	// 走 UploadBytes 管线：office 校验/扫描/配额/黑名单/版本/空间归属全部生效。
	session, err := deps.Uploads.UploadBytes(id.UserID, parent.ID, a.Name, data)
	if err != nil {
		return nil, err
	}
	if session.Status != upload.StatusAvailable {
		return nil, fail("upload finished with status %s", session.Status)
	}
	f, err := deps.Files.Get(id.UserID, session.FileID)
	if err != nil {
		return nil, err
	}
	out := fileJSON(f)
	if v, blob, verr := deps.Files.CurrentVersion(id.UserID, f.ID); verr == nil {
		out["current_version"] = currentVersionJSON(v, blob)
	}
	return out, nil
}

func toolWriteVersion(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID        string `json:"file_id"`
		ContentText   string `json:"content_text"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	data, err := decodeContent(a.ContentText, a.ContentBase64)
	if err != nil {
		return nil, err
	}
	// 覆盖为新版本：StartReplace（目标校验+配额）→ Append → Complete（校验/扫描）。
	session, err := deps.Uploads.StartReplace(id.UserID, fileID, int64(len(data)), "")
	if err != nil {
		return nil, err
	}
	if _, err = deps.Uploads.Append(session.ID, 0, bytes.NewReader(data)); err != nil {
		return nil, err
	}
	if _, err = deps.Uploads.Complete(session.ID); err != nil {
		return nil, err
	}
	f, err := deps.Files.Get(id.UserID, fileID)
	if err != nil {
		return nil, err
	}
	out := fileJSON(f)
	if v, blob, verr := deps.Files.CurrentVersion(id.UserID, fileID); verr == nil {
		out["current_version"] = currentVersionJSON(v, blob)
	}
	return out, nil
}

func toolReadFile(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID    string `json:"file_id"`
		VersionID string `json:"version_id"`
		As        string `json:"as"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	as := strings.ToLower(strings.TrimSpace(a.As))
	if as == "" {
		as = "text"
	}
	if as != "text" && as != "base64" {
		return nil, badArgs("as must be text or base64")
	}
	var name string
	var version files.FileVersion
	var blob files.ObjectBlob
	if a.VersionID == "" {
		f, gerr := deps.Files.Get(id.UserID, fileID)
		if gerr != nil {
			return nil, gerr
		}
		name = f.Name
		version, blob, err = deps.Files.CurrentVersion(id.UserID, fileID)
	} else {
		versionID, verr := parseUUIDField("version_id", a.VersionID)
		if verr != nil {
			return nil, verr
		}
		var f files.File
		f, version, blob, err = deps.Files.ReadVersion(id.UserID, fileID, versionID)
		name = f.Name
	}
	if err != nil {
		return nil, err
	}
	if blob.Status != files.BlobStatusAvailable {
		return nil, fail("version is not available (status: %s)", blob.Status)
	}
	if blob.Size > maxReadBytes {
		return nil, fail("file is %d bytes, exceeding the %d MB read limit; fetch it via GET /api/v1/files/%s/download with your Bearer credential", blob.Size, maxReadBytes>>20, fileID)
	}
	if as == "text" && !search.IsTextIndexable(blob.MimeType, name) {
		return nil, fail("content is binary (mime: %s); call again with as=base64", blob.MimeType)
	}
	reader, err := upload.ReadSection(deps.Storage, blob.StorageKey, 0, blob.Size)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	content := string(data)
	if as == "base64" {
		content = base64.StdEncoding.EncodeToString(data)
	}
	return map[string]any{
		"file_id": fileID, "version_id": version.ID, "name": name,
		"mime_type": blob.MimeType, "size": blob.Size, "encoding": as, "content": content,
	}, nil
}

func toolRenameFile(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID string `json:"file_id"`
		Name   string `json:"name"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Name) == "" {
		return nil, badArgs("name is required")
	}
	f, err := deps.Files.Rename(id.UserID, fileID, a.Name)
	if err != nil {
		return nil, err
	}
	return fileJSON(f), nil
}

func toolMoveFile(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID   string `json:"file_id"`
		ParentID string `json:"parent_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	parentID, err := parseUUIDField("parent_id", a.ParentID)
	if err != nil {
		return nil, err
	}
	// 复用批量移动单项语义（授权/冲突/后代判定由服务层保证）。
	results, err := deps.Files.BatchMove(id.UserID, []uuid.UUID{fileID}, parentID)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 || !results[0].OK {
		code := "INTERNAL"
		if len(results) > 0 {
			code = results[0].ErrorCode
		}
		return nil, fail("move failed: %s", batchCodeMessage(code))
	}
	return map[string]any{"file_id": fileID, "parent_id": parentID, "moved": true}, nil
}

func toolCopyFile(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID   string `json:"file_id"`
		ParentID string `json:"parent_id"`
		Name     string `json:"name"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	parentID, err := parseUUIDField("parent_id", a.ParentID)
	if err != nil {
		return nil, err
	}
	f, err := deps.Files.Copy(id.UserID, fileID, parentID, a.Name)
	if err != nil {
		return nil, err
	}
	return fileJSON(f), nil
}

func toolDeleteFile(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID string `json:"file_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	if err := deps.Files.Delete(id.UserID, fileID); err != nil {
		return nil, err
	}
	return map[string]any{"file_id": fileID, "deleted": true, "hint": "soft-deleted to trash; restore with df_restore_file"}, nil
}

func toolRestoreFile(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID string `json:"file_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	f, err := deps.Files.Restore(id.UserID, fileID)
	if err != nil {
		return nil, err
	}
	return fileJSON(f), nil
}

func toolListTrash(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID string `json:"space_id"`
		Limit   int    `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	spaceID, err := resolveSpaceID(deps, id.UserID, a.SpaceID)
	if err != nil {
		return nil, err
	}
	if rerr := requireSpaceRead(deps, id.UserID, spaceID); rerr != nil {
		return nil, rerr
	}
	out, err := deps.Files.ListTrashSpace(id.UserID, spaceID, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"space_id": spaceID, "files": filesJSONList(out), "count": len(out)}, nil
}

func toolListVersions(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID string `json:"file_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	versions, err := deps.Files.ListVersions(id.UserID, fileID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		items = append(items, versionDetailJSON(v))
	}
	return map[string]any{"file_id": fileID, "versions": items, "count": len(items)}, nil
}

func toolRestoreVersion(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID    string `json:"file_id"`
		VersionID string `json:"version_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	versionID, err := parseUUIDField("version_id", a.VersionID)
	if err != nil {
		return nil, err
	}
	f, version, err := deps.Files.SetCurrentVersion(id.UserID, fileID, versionID)
	if err != nil {
		return nil, err
	}
	out := fileJSON(f)
	out["current_version"] = currentVersionJSON(version, files.ObjectBlob{})
	return out, nil
}

func toolSearchFiles(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return nil, badArgs("query is required")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = search.QueryLimitDefault
	}
	results, err := deps.Search.Query(id.UserID, search.QueryOptions{Q: q, Limit: limit})
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(results))
	for _, r := range results {
		item := map[string]any{"id": r.ID, "name": r.Name, "type": r.Type, "parent_id": r.ParentID, "updated_at": r.UpdatedAt}
		if r.Snippet != "" {
			item["snippet"] = r.Snippet
		}
		items = append(items, item)
	}
	return map[string]any{"query": q, "results": items, "count": len(items)}, nil
}

func toolListShares(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		Limit int `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	out, err := deps.Shares.ListWithFileNames(id.UserID, limit)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(out))
	for _, it := range out {
		items = append(items, map[string]any{
			"id": it.Share.ID, "file_id": it.Share.FileID, "file_name": it.FileName,
			"visibility": it.Share.Visibility, "permission": it.Share.Permission,
			"has_password": it.Share.HasPassword(), "expires_at": it.Share.ExpiresAt,
			"max_downloads": it.Share.MaxDownloads, "download_count": it.Share.DownloadCount,
			"revoked_at": it.Share.RevokedAt, "created_at": it.Share.CreatedAt,
		})
	}
	return map[string]any{"shares": items, "count": len(items)}, nil
}

func toolCreateShare(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID        string   `json:"file_id"`
		Permission    string   `json:"permission"`
		Visibility    string   `json:"visibility"`
		ExpiresInDays *int     `json:"expires_in_days"`
		Password      string   `json:"password"`
		MaxDownloads  *int     `json:"max_downloads"`
		UserIDs       []string `json:"user_ids"`
		SpaceIDs      []string `json:"space_ids"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	permission := strings.TrimSpace(a.Permission)
	if permission == "" {
		permission = share.PermissionView
	}
	visibility := strings.TrimSpace(a.Visibility)
	if visibility == "" {
		visibility = share.VisibilityPublic
	}
	var expiresIn time.Duration
	if a.ExpiresInDays != nil {
		if *a.ExpiresInDays < 0 {
			return nil, badArgs("expires_in_days must be >= 0")
		}
		expiresIn = time.Duration(*a.ExpiresInDays) * 24 * time.Hour
	}
	opts := share.ShareOptions{Password: a.Password}
	if visibility == share.VisibilityPrivate {
		if a.Password != "" {
			return nil, badArgs("password is only allowed for public shares")
		}
		parseIDs := func(values []string, name string) ([]uuid.UUID, error) {
			out := make([]uuid.UUID, 0, len(values))
			for _, v := range values {
				u, perr := parseUUIDField(name, v)
				if perr != nil {
					return nil, perr
				}
				out = append(out, u)
			}
			return out, nil
		}
		userIDs, uerr := parseIDs(a.UserIDs, "user_ids")
		if uerr != nil {
			return nil, uerr
		}
		spaceIDs, serr := parseIDs(a.SpaceIDs, "space_ids")
		if serr != nil {
			return nil, serr
		}
		created, err := deps.Shares.CreatePrivateWithOptions(id.UserID, fileID, permission, expiresIn, a.MaxDownloads, userIDs, spaceIDs, opts)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"share_id": created.ID, "file_id": created.FileID, "visibility": share.VisibilityPrivate,
			"permission": created.Permission, "expires_at": created.ExpiresAt, "created_at": created.CreatedAt,
		}, nil
	}
	if visibility != share.VisibilityPublic {
		return nil, badArgs("visibility must be public or private")
	}
	if len(a.UserIDs) > 0 || len(a.SpaceIDs) > 0 {
		return nil, badArgs("user_ids/space_ids are only allowed for private shares")
	}
	created, token, err := deps.Shares.CreatePublic(id.UserID, fileID, permission, expiresIn, a.MaxDownloads, opts)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"share_id": created.ID, "file_id": created.FileID, "visibility": share.VisibilityPublic,
		"permission": created.Permission, "has_password": created.HasPassword(),
		"expires_at": created.ExpiresAt, "created_at": created.CreatedAt,
		"token": token, "share_url": "/api/v1/public/shares/" + token,
	}, nil
}

func toolRevokeShare(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		ShareID string `json:"share_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	shareID, err := parseUUIDField("share_id", a.ShareID)
	if err != nil {
		return nil, err
	}
	revoked, err := deps.Shares.Revoke(id.UserID, shareID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"share_id": revoked.ID, "file_id": revoked.FileID, "revoked": true}, nil
}

func toolListSpaces(_ context.Context, deps *Deps, id Identity, _ json.RawMessage) (any, error) {
	spaces, err := deps.Spaces.ListSpaces(id.UserID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(spaces))
	for _, sp := range spaces {
		items = append(items, map[string]any{
			"id": sp.ID, "name": sp.Name, "description": sp.Description,
			"owner_id": sp.OwnerID, "is_default": sp.IsDefault, "created_at": sp.CreatedAt,
		})
	}
	return map[string]any{"spaces": items, "count": len(items)}, nil
}

func toolResolvePath(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		SpaceID string `json:"space_id"`
		Path    string `json:"path"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	spaceID, err := resolveSpaceID(deps, id.UserID, a.SpaceID)
	if err != nil {
		return nil, err
	}
	// 复用 resolve 链路（逐段解析 + authorizeFileAccess 空间 ACL/成员判定）。
	f, chain, err := deps.Files.ResolveReadablePath(id.UserID, files.NamespaceSpace, spaceID, a.Path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(chain))
	for _, c := range chain {
		names = append(names, c.Name)
	}
	return map[string]any{
		"file_id": f.ID, "name": f.Name, "type": f.Type,
		"space_id": spaceID, "canonical_path": strings.Join(names[1:], "/"),
	}, nil
}

func toolDownloadURL(_ context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID string `json:"file_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	// Get 自带读授权（个人 owner / 团队成员）。
	if _, err := deps.Files.Get(id.UserID, fileID); err != nil {
		return nil, err
	}
	return map[string]any{
		"file_id":      fileID,
		"download_url": "/api/v1/files/" + fileID.String() + "/download",
		"preview_url":  "/api/v1/files/" + fileID.String() + "/preview",
		"auth_hint":    "send Authorization: Bearer <dfpat_ PAT or access token>",
	}, nil
}

// ---------- 错误信息映射 ----------

// errorMessage 把服务层错误转为 agent 可读信息（sentinel 映射 + userError
// 透传；未知错误统一文案，不泄露内部细节，与 HTTP 层 default 分支一致）。
func errorMessage(tool string, err error) string {
	msg := "operation failed"
	switch {
	case errors.Is(err, errToolUnavailable):
		msg = "this capability is not available on this deployment (server did not enable the required service)"
	case errors.Is(err, files.ErrInvalidName):
		msg = "invalid name (empty, illegal/invisible characters, reserved device name, trailing dot, or longer than 255 characters)"
	case errors.Is(err, files.ErrConflict):
		msg = "name conflict: an item with the same name already exists in the target folder"
	case errors.Is(err, files.ErrForbidden):
		msg = "forbidden: no permission for this operation (space role or folder ACL denied)"
	case errors.Is(err, files.ErrNotFound):
		msg = "not found: the file/folder/share does not exist, or you have no access to it"
	case errors.Is(err, files.ErrRoot):
		msg = "the root folder cannot be changed"
	case errors.Is(err, files.ErrNoVersion):
		msg = "the file has no current version"
	case errors.Is(err, files.ErrFolderDepth):
		msg = "folder depth limit exceeded"
	case errors.Is(err, files.ErrFolderCopy):
		msg = "folders cannot be copied (files only)"
	case errors.Is(err, files.ErrQuotaExceeded):
		msg = "storage quota exceeded"
	case errors.Is(err, files.ErrParentDeleted):
		msg = "the parent folder is in the trash"
	case errors.Is(err, files.ErrNotDeleted):
		msg = "the item is not in the trash"
	case errors.Is(err, files.ErrMoveTarget):
		msg = "invalid move target (cannot move into itself or its descendant)"
	case errors.Is(err, files.ErrNotFileVersion):
		msg = "the version does not belong to this file"
	case errors.Is(err, files.ErrCurrentVersion):
		msg = "the current version cannot be deleted"
	case errors.Is(err, upload.ErrInvalidOffice), errors.Is(err, upload.ErrBlockedExtension),
		errors.Is(err, upload.ErrSize), errors.Is(err, upload.ErrTooManyUploads),
		errors.Is(err, upload.ErrTargetUnavailable):
		msg = err.Error()
	case errors.Is(err, ai.ErrNoProvider):
		msg = "no ai provider is configured on this deployment (ask the administrator to add one in admin settings)"
	case errors.Is(err, ai.ErrProviderNotFound):
		msg = "the specified ai provider does not exist or is disabled"
	case errors.Is(err, ai.ErrUpstreamChat), errors.Is(err, ai.ErrUpstream):
		msg = "the ai upstream request failed (network, timeout, or provider error)"
	case errors.Is(err, share.ErrInvalidPermission), errors.Is(err, share.ErrInvalidExpiry),
		errors.Is(err, share.ErrInvalidVisibility), errors.Is(err, share.ErrInvalidPassword),
		errors.Is(err, share.ErrPublicDisabled), errors.Is(err, share.ErrNotFound),
		errors.Is(err, share.ErrFileNotFound), errors.Is(err, share.ErrFileNotShareable),
		errors.Is(err, share.ErrForbidden):
		msg = err.Error()
	default:
		var ue *userError
		if errors.As(err, &ue) {
			msg = ue.message
			break
		}
		msg = "operation failed (internal error)"
	}
	return tool + ": " + msg
}

// batchCodeMessage 把批量移动单项错误码转为可读信息。
func batchCodeMessage(code string) string {
	switch code {
	case files.BatchCodeNotFound:
		return "file or target folder not found (or no access)"
	case files.BatchCodeForbidden:
		return "no permission to move this item"
	case files.BatchCodeRoot:
		return "the root folder cannot be moved"
	case files.BatchCodeNameConflict:
		return "name conflict in the target folder"
	case files.BatchCodeInvalidTarget:
		return "invalid target (cannot move into itself or its descendant)"
	case files.BatchCodeParentDeleted:
		return "the target folder is in the trash"
	case files.BatchCodeDepthLimit:
		return "folder depth limit exceeded"
	default:
		return "internal error"
	}
}
