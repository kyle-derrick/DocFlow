// Package http —— ai_platform_tools.go：内置平台文件工具执行桥（use_files）。
//
// /ai/chat 开启 use_files 后，HTTP 层把 internal/ai 声明的 df_* 工具
// （PlatformTools）注入 ChatRequest 并挂接本文件的 executePlatformTool
// 回调——ai 包不依赖 http/service 层（回调注入解耦）。执行全部直调
// service 层既有链路（对齐 MCP 官方 filesystem server 范式，不经 MCP HTTP）：
//   - 列目录/建目录/路径解析：files（ListSpace/CreateFolderIn/FindChildByName）；
//   - 读文件：files.CurrentVersion + storage 读 + ai.ExtractText 文本抽取；
//   - 写文件：upload 管线（新建走 UploadBytes；覆盖走 StartReplace→Append→
//     Complete 的「上传 file_id 覆盖新版本」链路——drawio 保存同链路，
//     版本链自动保护、旧版本可回滚；office 校验/病毒扫描/配额/黑名单
//     均自然生效）；
//   - 搜索：search 全文检索（top 8：名称+snippet）。
//
// 每次工具执行写一条 ai.tool 审计（工具名+参数摘要+结果状态，照 aiChat
// 的 audit 写法）。安全边界见 internal/ai/platformtools.go（无 delete/move）。
package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/space"
	"github.com/docflow/docflow/internal/upload"
)

// aiToolAuditAction 内置平台文件工具执行的审计动作（action "ai.tool"，
// 对齐 aiChat 的 audit.ActionAIChat 写法；常量放本文件避免改动 audit 包）。
const aiToolAuditAction = "ai.tool"

// platformFileStore 内置平台文件工具所需的最小文件能力（生产为
// *files.Store；接口化便于单测注入内存实现，模式同 resolver/unpacker——
// 测试侧经 h.mcpDeps.Files 或直接构造 platformToolCtx 注入）。
type platformFileStore interface {
	// Get 读单个文件/目录（authorizeFileAccess 读授权）。
	Get(user, fileID uuid.UUID) (files.File, error)
	// CurrentVersion 当前版本及其 blob（内部经 Get 授权）。
	CurrentVersion(user, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
	// CurrentBlobs 批量当前版本 blob 元数据（列目录聚合 size 用）。
	CurrentBlobs(ids []uuid.UUID) map[uuid.UUID]files.ObjectBlob
	// CreateFolderIn 建子目录（空间写授权 + 深度校验）。
	CreateFolderIn(user, parent uuid.UUID, name string) (files.File, error)
	// ListSpace 列目录直接子项。
	ListSpace(spaceID, parent uuid.UUID, limit int, f files.SpaceListFilter) ([]files.File, error)
	// FindChildByName 按名查直接子项（存在性判定：写文件区分新建/覆盖）。
	FindChildByName(parent uuid.UUID, name string) (files.File, error)
	// SpaceRoot 空间根目录。
	SpaceRoot(spaceID uuid.UUID) (files.File, error)
}

// 编译期断言：生产 *files.Store 满足本接口。
var _ platformFileStore = (*files.Store)(nil)

// platformToolServices 执行器所需的 service 层依赖切片。
type platformToolServices struct {
	store   platformFileStore // 文件树（生产 h.files）
	uploads *upload.Service   // 上传管线（写文件；生产 h.uploads）
	storage upload.Storage    // 对象存储（读文件内容；生产 h.storage）
	spaces  *space.Service    // 默认空间解析（生产 h.spaces）
	search  searchService     // 全文检索（df_search；可 nil = 未启用）
}

// platformToolCtx 为一次 use_files 对话的执行上下文（aiChat 每请求构造；
// c 仅用于审计 IP/UA 采集，工具在请求内同步执行，无生命周期问题）。
type platformToolCtx struct {
	h    *Handler
	c    *gin.Context
	user uuid.UUID
	// workRoot 为工作目录 folderID（相对路径的解析基准）；零值 = 用户
	// 默认空间根目录（当前唯一取值，预留按请求指定工作目录的扩展位）。
	workRoot uuid.UUID
	svc      platformToolServices
}

// platformFileStore 解析内置工具的文件源：优先 h.files（生产装配），
// 回退 h.mcpDeps.Files（测试注入内存实现的既有通道，模式同 resolver）。
// 无法解析返回 nil（use_files 静默跳过）。
func (h *Handler) platformFileStore() platformFileStore {
	if h.files != nil {
		return h.files
	}
	if h.mcpDeps != nil {
		if s, ok := h.mcpDeps.Files.(platformFileStore); ok {
			return s
		}
	}
	return nil
}

// newPlatformToolCtx 构造执行上下文：依赖（文件源/上传/存储/空间服务）
// 未装配或用户无默认空间（= 无任何空间）时返回 nil——调用方静默不注入
// df_* 工具（对话不中断）。
func (h *Handler) newPlatformToolCtx(c *gin.Context, user, workRoot uuid.UUID) *platformToolCtx {
	store := h.platformFileStore()
	if store == nil || h.uploads == nil || h.storage == nil || h.spaces == nil {
		return nil
	}
	if _, err := h.spaces.DefaultSpace(user); err != nil {
		return nil // 用户没有任何空间（默认空间不存在）：工具不可用
	}
	return &platformToolCtx{
		h: h, c: c, user: user, workRoot: workRoot,
		svc: platformToolServices{store: store, uploads: h.uploads, storage: h.storage, spaces: h.spaces, search: h.search},
	}
}

// ---------- 工具实现 ----------

// base 解析相对路径基准目录：显式 workRoot（读授权经 Get）或用户默认
// 空间根目录（DefaultSpace 已限定本人空间，天然授权）。
func (p *platformToolCtx) base() (files.File, error) {
	if p.workRoot != uuid.Nil {
		f, err := p.svc.store.Get(p.user, p.workRoot)
		if err != nil {
			return files.File{}, fmt.Errorf("工作目录不可访问")
		}
		if f.Type != "folder" {
			return files.File{}, fmt.Errorf("工作目录不是目录")
		}
		return f, nil
	}
	sp, err := p.svc.spaces.DefaultSpace(p.user)
	if err != nil {
		return files.File{}, fmt.Errorf("无法解析默认空间")
	}
	root, err := p.svc.store.SpaceRoot(sp.ID)
	if err != nil {
		return files.File{}, fmt.Errorf("无法解析默认空间根目录")
	}
	return root, nil
}

// walk 自 base 逐段下潜（FindChildByName；中间段必须是目录），返回命中
// 行；任何断链/不存在统一 files.ErrNotFound（不泄露细节）。
func (p *platformToolCtx) walk(base files.File, segments []string) (files.File, error) {
	cur := base
	for i, seg := range segments {
		child, err := p.svc.store.FindChildByName(cur.ID, seg)
		if err != nil {
			return files.File{}, files.ErrNotFound
		}
		if i < len(segments)-1 && child.Type != "folder" {
			return files.File{}, files.ErrNotFound
		}
		cur = child
	}
	return cur, nil
}

// segmentsOf 校验并拆分相对路径（NFC/非法字符/段数上限走 files 既有校验）。
func segmentsOf(path string) ([]string, error) {
	segments, err := files.SplitPathSegments(strings.TrimSpace(path))
	if err != nil {
		return nil, fmt.Errorf("路径不合法：%v", err)
	}
	return segments, nil
}

// toolListDir df_list_dir：列目录（名称/类型/大小/updated_at，目录 size=0），
// 上限 ai.PlatformToolListLimit 条，超出标注 truncated。
func (p *platformToolCtx) toolListDir(path string) (any, error) {
	base, err := p.base()
	if err != nil {
		return nil, err
	}
	dir := base
	display := ""
	if strings.TrimSpace(path) != "" {
		segments, err := segmentsOf(path)
		if err != nil {
			return nil, err
		}
		f, err := p.walk(base, segments)
		if err != nil || f.Type != "folder" {
			return nil, fmt.Errorf("目录不存在或无权限访问")
		}
		// 终项读授权（walk 不鉴权，与 sharetree 的 ResolveSubpath 口径一致）。
		if _, err := p.svc.store.Get(p.user, f.ID); err != nil {
			return nil, fmt.Errorf("目录不存在或无权限访问")
		}
		dir = f
		display = strings.Join(segments, "/")
	}
	items, err := p.svc.store.ListSpace(dir.SpaceID, dir.ID, ai.PlatformToolListLimit, files.SpaceListFilter{})
	if err != nil {
		return nil, fmt.Errorf("目录列举失败")
	}
	ids := make([]uuid.UUID, 0, len(items))
	for _, f := range items {
		if f.Type == "file" {
			ids = append(ids, f.ID)
		}
	}
	blobs := p.svc.store.CurrentBlobs(ids)
	entries := make([]map[string]any, 0, len(items))
	for _, f := range items {
		size := int64(0)
		if f.Type == "file" {
			if b, ok := blobs[f.ID]; ok {
				size = b.Size
			}
		}
		entries = append(entries, map[string]any{
			"name": f.Name, "type": f.Type, "size": size, "updated_at": f.UpdatedAt,
		})
	}
	return gin.H{"path": display, "entries": entries, "count": len(entries), "truncated": len(items) >= ai.PlatformToolListLimit}, nil
}

// toolReadFile df_read_file：读文件文本（复用 ai.ExtractText 抽取链路：
// 文本直读，pdf/docx/xlsx/pptx/drawio/excalidraw/dfdoc 转文本），截
// ai.PlatformToolReadMaxRunes 字符。
func (p *platformToolCtx) toolReadFile(path string) (any, error) {
	segments, err := segmentsOf(path)
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("path 不能为空")
	}
	base, err := p.base()
	if err != nil {
		return nil, err
	}
	f, werr := p.walk(base, segments)
	if werr != nil || f.Type != "file" {
		return nil, fmt.Errorf("文件不存在或无权限访问")
	}
	_, blob, err := p.svc.store.CurrentVersion(p.user, f.ID) // 内部经 Get 读授权
	if err != nil {
		return nil, fmt.Errorf("文件不存在或无权限访问")
	}
	if blob.Status != files.BlobStatusAvailable {
		return nil, fmt.Errorf("文件当前版本尚未就绪（状态 %s）", blob.Status)
	}
	if blob.StorageKey == "" {
		return nil, fmt.Errorf("文件无内容")
	}
	r, err := p.svc.storage.Read(blob.StorageKey)
	if err != nil {
		return nil, fmt.Errorf("文件内容读取失败")
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, ai.MaxExtractBytes))
	if err != nil {
		return nil, fmt.Errorf("文件内容读取失败")
	}
	text, ok := ai.ExtractText(f.Name, blob.MimeType, data)
	if !ok {
		return nil, fmt.Errorf("该文件类型暂不支持文本读取（二进制或抽取失败）")
	}
	truncated := false
	if runes := []rune(text); len(runes) > ai.PlatformToolReadMaxRunes {
		text = string(runes[:ai.PlatformToolReadMaxRunes])
		truncated = true
	}
	return gin.H{"path": strings.Join(segments, "/"), "name": f.Name, "size": blob.Size, "truncated": truncated, "content": text}, nil
}

// replaceBytes 覆盖既有文件为新版本：StartReplace→Append→Complete 组合
// （与 drawio 保存同一条「上传 file_id 覆盖新版本」管线；写授权在
// StartReplace 的 validateTarget 内判定）。
func (p *platformToolCtx) replaceBytes(fileID uuid.UUID, data []byte) error {
	v, err := p.svc.uploads.StartReplace(p.user, fileID, int64(len(data)), "")
	if err != nil {
		return err
	}
	if _, err = p.svc.uploads.Append(v.ID, 0, bytes.NewReader(data)); err != nil {
		return err
	}
	if _, err = p.svc.uploads.Complete(v.ID); err != nil {
		return err
	}
	return nil
}

// toolWriteFile df_write_file：创建或覆盖文本文件（扩展名白名单 + ≤2MB；
// 覆盖自动留版本；父目录须已存在，建议先 df_mkdir）。
func (p *platformToolCtx) toolWriteFile(path, content string) (any, error) {
	segments, err := segmentsOf(path)
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("path 不能为空")
	}
	name := segments[len(segments)-1]
	if !ai.PlatformToolWriteExtAllowed(name) {
		return nil, fmt.Errorf("扩展名不在允许的文本类型白名单内（md/txt/代码/json/yaml/html/svg/drawio 等），拒绝写入二进制或未知类型")
	}
	data := []byte(content)
	if int64(len(data)) > ai.PlatformToolWriteMaxBytes {
		return nil, fmt.Errorf("内容超过 %dMB 上限", ai.PlatformToolWriteMaxBytes>>20)
	}
	base, err := p.base()
	if err != nil {
		return nil, err
	}
	parent, werr := p.walk(base, segments[:len(segments)-1])
	if werr != nil || parent.Type != "folder" {
		return nil, fmt.Errorf("父目录不存在（可先用 df_mkdir 创建）")
	}
	created := false
	var fileID uuid.UUID
	if target, terr := p.svc.store.FindChildByName(parent.ID, name); terr == nil {
		// 覆盖既有文件：同名目录拒绝，文件走版本覆盖链路。
		if target.Type != "file" {
			return nil, fmt.Errorf("同名目录已存在，无法写入文件")
		}
		if rerr := p.replaceBytes(target.ID, data); rerr != nil {
			return nil, fmt.Errorf("覆盖写入失败：%v", rerr)
		}
		fileID = target.ID
	} else {
		// 新建文件：完整上传管线（校验/扫描/配额/黑名单生效）。
		sess, uerr := p.svc.uploads.UploadBytes(p.user, parent.ID, name, data)
		if uerr != nil {
			return nil, fmt.Errorf("写入失败：%v", uerr)
		}
		fileID = sess.FileID
		created = true
	}
	return gin.H{"path": strings.Join(segments, "/"), "file_id": fileID, "created": created}, nil
}

// toolMkdir df_mkdir：多级建目录（已存在的中间目录跳过；同名文件报错）。
func (p *platformToolCtx) toolMkdir(path string) (any, error) {
	segments, err := segmentsOf(path)
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("path 不能为空")
	}
	base, err := p.base()
	if err != nil {
		return nil, err
	}
	cur := base
	for _, seg := range segments {
		child, cerr := p.svc.store.FindChildByName(cur.ID, seg)
		switch {
		case cerr == nil:
			if child.Type != "folder" {
				return nil, fmt.Errorf("路径段 %q 是同名文件，无法作为目录", seg)
			}
			cur = child
		case errors.Is(cerr, files.ErrNotFound):
			created, kerr := p.svc.store.CreateFolderIn(p.user, cur.ID, seg)
			if kerr != nil {
				if errors.Is(kerr, files.ErrForbidden) {
					return nil, fmt.Errorf("无权限在该目录下创建子目录")
				}
				if errors.Is(kerr, files.ErrConflict) {
					return nil, fmt.Errorf("目录 %q 创建冲突（同名项已存在）", seg)
				}
				return nil, fmt.Errorf("目录创建失败：%v", kerr)
			}
			cur = created
		default:
			return nil, fmt.Errorf("目录解析失败：%v", cerr)
		}
	}
	return gin.H{"path": strings.Join(segments, "/")}, nil
}

// toolSearch df_search：平台全文检索 top ai.PlatformToolSearchLimit
// （名称 + 内容命中片段）。
func (p *platformToolCtx) toolSearch(query string) (any, error) {
	if p.svc.search == nil {
		return nil, fmt.Errorf("平台未启用全文搜索")
	}
	results, err := p.svc.search.Query(p.user, search.QueryOptions{Q: query, Limit: ai.PlatformToolSearchLimit})
	if err != nil {
		return nil, fmt.Errorf("搜索失败")
	}
	entries := make([]map[string]any, 0, len(results))
	for _, r := range results {
		entries = append(entries, map[string]any{
			"id": r.ID, "name": r.Name, "type": r.Type, "snippet": r.Snippet, "updated_at": r.UpdatedAt,
		})
	}
	return gin.H{"query": query, "results": entries, "count": len(entries)}, nil
}

// ---------- 分发与审计 ----------

// executePlatformTool 执行一个内置工具调用（chat.go 工具循环经
// ChatRequest.ToolExecutor 回调到此处）。参数非法/业务失败返回
// {"ok":false,"error":"..."} JSON（作为工具结果回喂模型，不中断对话）；
// 仅未知工具名返回 Go 错误（chat.go 转为「工具调用失败」）。每次执行
// 写一条 ai.tool 审计（工具名+参数摘要+结果状态+耗时）。
func (p *platformToolCtx) executePlatformTool(name string, argsJSON json.RawMessage) (string, error) {
	args := map[string]any{}
	if len(strings.TrimSpace(string(argsJSON))) > 0 {
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return platformToolResult(gin.H{"error": "参数不是合法 JSON 对象"}), nil
		}
	}
	str := func(key string) string {
		v, _ := args[key].(string)
		return strings.TrimSpace(v)
	}
	started := time.Now()
	var summary string
	var result any
	var err error
	switch name {
	case ai.PlatformToolListDir:
		path := str("path")
		summary = "path=" + path
		result, err = p.toolListDir(path)
	case ai.PlatformToolReadFile:
		path := str("path")
		summary = "path=" + path
		if path == "" {
			err = fmt.Errorf("path 为必填参数")
		} else {
			result, err = p.toolReadFile(path)
		}
	case ai.PlatformToolWriteFile:
		path, content := str("path"), ""
		if raw, ok := args["content"].(string); ok {
			content = raw
		}
		summary = fmt.Sprintf("path=%s content_bytes=%d", path, len(content))
		if path == "" || !jsonHas(args, "content") {
			err = fmt.Errorf("path 与 content 为必填参数")
		} else {
			result, err = p.toolWriteFile(path, content)
		}
	case ai.PlatformToolMkdir:
		path := str("path")
		summary = "path=" + path
		if path == "" {
			err = fmt.Errorf("path 为必填参数")
		} else {
			result, err = p.toolMkdir(path)
		}
	case ai.PlatformToolSearch:
		query := str("query")
		summary = "query=" + query
		if query == "" {
			err = fmt.Errorf("query 为必填参数")
		} else {
			result, err = p.toolSearch(query)
		}
	default:
		return "", fmt.Errorf("未知内置工具 %q", name)
	}
	status := audit.StatusSuccess
	if err != nil {
		status = audit.StatusFailure
	}
	p.auditTool(name, status, summary, time.Since(started).Milliseconds())
	if err != nil {
		return platformToolResult(gin.H{"error": err.Error()}), nil
	}
	out, merr := json.Marshal(result)
	if merr != nil {
		return platformToolResult(gin.H{"error": "工具结果序列化失败"}), nil
	}
	return `{"ok":true,` + strings.TrimPrefix(string(out), "{"), nil
}

// jsonHas 判定参数键存在（区分「缺省」与「空串」——content 允许空内容）。
func jsonHas(args map[string]any, key string) bool {
	_, ok := args[key]
	return ok
}

// platformToolResult 包装失败结果为 {"ok":false,...} JSON 字符串。
func platformToolResult(failure gin.H) string {
	failure["ok"] = false
	raw, _ := json.Marshal(failure)
	return string(raw)
}

// auditTool 写一条工具执行审计（照 aiChat 的 recordAudit 写法：best-effort，
// 失败不影响主流程；metadata 含工具名/参数摘要/结果状态/耗时）。
func (p *platformToolCtx) auditTool(name, status, summary string, durationMS int64) {
	if p.h == nil {
		return
	}
	meta, _ := json.Marshal(map[string]any{
		"tool": name, "args": summary, "status": status, "duration_ms": durationMS,
	})
	entry := audit.Entry{
		UserID:       &p.user,
		Action:       aiToolAuditAction,
		ResourceType: audit.ResourceAI,
		Status:       status,
		Metadata:     string(meta),
	}
	if p.c != nil {
		p.h.recordAudit(p.c, entry)
		return
	}
	if entry.Status == "" {
		entry.Status = audit.StatusSuccess
	}
	_ = p.h.audit.Record(entry)
}
