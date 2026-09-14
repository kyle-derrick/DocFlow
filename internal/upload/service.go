package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/metrics"
	"github.com/google/uuid"
)

var (
	ErrNotFound = errors.New("upload session not found")
	ErrOffset   = errors.New("invalid upload offset")
	ErrSize     = errors.New("invalid upload size")
	ErrExpired  = errors.New("upload session expired")
	ErrChecksum = errors.New("checksum mismatch")
	ErrHash     = errors.New("invalid SHA-256 hash")
	ErrRejected = errors.New("scan rejected")
	ErrFailed   = errors.New("upload session failed")
	// ErrTargetUnavailable 版本覆盖能力未注入（服务未接线 files.Store 钩子）。
	ErrTargetUnavailable = errors.New("version target is not available")
	// ErrPatchTooLarge 单次 PATCH 请求体超过 per-request 上限
	//（SetPatchMaxBytes，默认 DefaultPatchMaxBytes=64MiB）。
	ErrPatchTooLarge = errors.New("request body exceeds per-request upload limit")
	// ErrInProgress 会话正在被另一方补完（CAS 状态迁移竞争失败且对方仍在
	// verifying/scanning 阶段）；调用方可稍后重试或轮询终态。
	ErrInProgress = errors.New("upload completion already in progress")
)

// DefaultPatchMaxBytes 单次 PATCH 请求体的默认上限（64MiB）：
// 流式复制不再整读缓冲，该上限约束单请求的存储写入量与连接占用时长。
const DefaultPatchMaxBytes int64 = 64 << 20

// sessionStore 为会话持久层的最小接口；并发正确性依赖以下可选能力，
// 由 MemoryStore / GormStore 实现（服务侧类型断言检测）：
type sessionStore interface {
	Save(UploadSession) error
	Get(uuid.UUID) (UploadSession, error)
	Update(UploadSession) error
}

// offsetCaser 为 sessionStore 的可选能力：offset 的条件更新（Append CAS）。
type offsetCaser interface {
	AdvanceOffset(id uuid.UUID, from, delta int64, now time.Time) error
}

// stateMarker 为 sessionStore 的可选能力：状态迁移的条件更新（Complete CAS）。
type stateMarker interface {
	MarkVerifying(id uuid.UUID, size int64) (bool, error)
	MarkScanning(id uuid.UUID) (bool, error)
	// MarkAvailable 同时落库终态 storage_key（tmp → objects/* 的迁移结果）。
	MarkAvailable(id uuid.UUID, storageKey string, completedAt time.Time) (bool, error)
}

type MemoryStore struct {
	mu    sync.RWMutex
	items map[uuid.UUID]UploadSession
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{items: make(map[uuid.UUID]UploadSession)} }
func (s *MemoryStore) Save(v UploadSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[v.ID] = v
	return nil
}
func (s *MemoryStore) Get(id uuid.UUID) (UploadSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[id]
	if !ok {
		return UploadSession{}, ErrNotFound
	}
	return v, nil
}
func (s *MemoryStore) Update(v UploadSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[v.ID]; !ok {
		return ErrNotFound
	}
	s.items[v.ID] = v
	return nil
}

func (s *MemoryStore) AdvanceOffset(id uuid.UUID, from, delta int64, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[id]
	if !ok {
		return ErrNotFound
	}
	if v.Status != StatusUploading || v.Offset != from {
		return ErrOffset
	}
	v.Offset += delta
	s.items[id] = v
	return nil
}

func (s *MemoryStore) MarkVerifying(id uuid.UUID, size int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[id]
	if !ok {
		return false, ErrNotFound
	}
	if v.Status != StatusUploading || v.Offset != size {
		return false, nil
	}
	v.Status = StatusVerifying
	s.items[id] = v
	return true, nil
}

func (s *MemoryStore) MarkScanning(id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[id]
	if !ok {
		return false, ErrNotFound
	}
	if v.Status != StatusVerifying {
		return false, nil
	}
	v.Status = StatusScanning
	s.items[id] = v
	return true, nil
}

func (s *MemoryStore) MarkAvailable(id uuid.UUID, storageKey string, completedAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.items[id]
	if !ok {
		return false, ErrNotFound
	}
	if v.Status != StatusScanning {
		return false, nil
	}
	v.Status = StatusAvailable
	v.StorageKey = storageKey
	v.CompletedAt = &completedAt
	s.items[id] = v
	return true, nil
}

type Service struct {
	store          sessionStore
	storage        Storage
	ttl            time.Duration
	maxSize        int64
	scanner        Scanner
	validateParent func(uuid.UUID, uuid.UUID) error
	// createFile 落库上传完成的新文件，返回新建文件 ID 与是否新建了 blob
	//（false 表示同 sha256 内容去重复用既有 blob，新物理对象冗余应删除）。
	createFile func(uuid.UUID, uuid.UUID, string, string, int64, string, string) (uuid.UUID, bool, error)
	// fileComplete 在文件落库成功（新文件或覆盖新版本）后同步回调一次，
	// 供网页包自动解包等后置处理注入；回调不改变会话终态（错误由注入方自负）。
	fileComplete func(uuid.UUID)
	// validateTarget 校验「覆盖为新版本」目标并返回文件行（type=file、未删除、
	// user 有 CanWrite 权限），由 files 包注入；会话沿用其现有名称与父目录。
	validateTarget func(uuid.UUID, uuid.UUID) (files.File, error)
	// replaceFile 在 Complete 成功时向目标文件追加新版本（AddVersion+Prune），
	// 返回是否新建了 blob（false 表示命中去重复用，新物理对象冗余应删除）。
	replaceFile func(uuid.UUID, uuid.UUID, string, string, int64, string) (bool, error)
	// notifyDispatcher 站内通知回调（main 注入 notify.Dispatcher 适配器）：
	// 完成路径（新文件/覆盖新版本成功）与隔离终态各回调一次；回调错误由
	// 注入方自理，不影响会话终态。eventType 见 notify 包常量
	//（upload.completed / upload.quarantined），resourceID=uuid.Nil 表示无资源。
	notifyDispatcher NotifyFunc
	// patchMax 单次 PATCH 请求体上限（0 = 不限）。
	patchMax int64
	// maxSizeProvider 单文件大小上限热读取（system_settings 的
	// upload.max_file_size，main 注入）；nil 或返回非正值时回退 maxSize。
	maxSizeProvider func() int64
	// sessionLocksMu 保护 sessionLocks；per-session 互斥在进程内串行化
	// 同一会话的 Append/Complete（双保险之一，跨进程由 store 侧 CAS 兜底）。
	sessionLocksMu sync.Mutex
	sessionLocks   map[uuid.UUID]*sync.Mutex
}

func NewService(store sessionStore, storage Storage, ttl time.Duration, maxSize int64, scanEnabled bool, validateParent func(uuid.UUID, uuid.UUID) error, createFile func(uuid.UUID, uuid.UUID, string, string, int64, string, string) (uuid.UUID, bool, error)) *Service {
	var scanner Scanner = AllowScanner{}
	if scanEnabled {
		scanner = RejectScanner{}
	}
	return &Service{store: store, storage: storage, ttl: ttl, maxSize: maxSize, scanner: scanner, validateParent: validateParent, createFile: createFile, patchMax: DefaultPatchMaxBytes, sessionLocks: make(map[uuid.UUID]*sync.Mutex)}
}
func (s *Service) SetScanner(scanner Scanner) {
	if scanner != nil {
		s.scanner = scanner
	}
}

// SetPatchMaxBytes 设置单次 PATCH 请求体上限（须 > 0 才生效；默认
// DefaultPatchMaxBytes=64MiB，对应部署环境变量 PATCH_MAX_BYTES 的接线点）。
func (s *Service) SetPatchMaxBytes(n int64) {
	if n > 0 {
		s.patchMax = n
	}
}

// PatchMaxBytes 返回单次 PATCH 请求体上限（0 = 不限）；HTTP 层据此对
// 超限的 Content-Length 直接回 413。
func (s *Service) PatchMaxBytes() int64 { return s.patchMax }

// SetMaxSizeProvider 注入单文件大小上限的热读取（每次建会话时调用；
// 供 system_settings 的 upload.max_file_size 运行时覆盖 MAX_FILE_SIZE）。
// 返回非正值（读失败/非法值时由注入方自行回退）保持构造值。
func (s *Service) SetMaxSizeProvider(fn func() int64) {
	if fn != nil {
		s.maxSizeProvider = fn
	}
}

// effectiveMaxSize 返回当前生效的单文件大小上限：provider 热读取优先，
// 未注入或返回非正值时回退构造值（MAX_FILE_SIZE）。
func (s *Service) effectiveMaxSize() int64 {
	if s.maxSizeProvider != nil {
		if n := s.maxSizeProvider(); n > 0 {
			return n
		}
	}
	return s.maxSize
}

// lockSession 按 sessionID 加锁并返回解锁函数：进程内串行化同一会话的
// Append/Complete，防止并发 PATCH 交错写入。多实例（redis 队列/水平扩展）
// 部署下进程间无互斥，由 store 侧 CAS（AdvanceOffset / MarkVerifying）
// 兜底：竞争失败方回退预占或按重读到的状态返回幂等/冲突语义。
// 锁表条目随会话生命周期常驻（janitor 清理终态会话后为少量死条目，
// 单条仅一个互斥量指针，量级与会话数一致，可接受）。
func (s *Service) lockSession(id uuid.UUID) func() {
	s.sessionLocksMu.Lock()
	mu, ok := s.sessionLocks[id]
	if !ok {
		mu = &sync.Mutex{}
		s.sessionLocks[id] = mu
	}
	s.sessionLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// SetFileCompleteHook 注入「文件落库完成」回调（新文件与覆盖新版本均触发，
// 参数为文件 ID）；注入的回调自行决定同步/异步执行策略。
func (s *Service) SetFileCompleteHook(fn func(fileID uuid.UUID)) {
	if fn != nil {
		s.fileComplete = fn
	}
}

// NotifyFunc 站内通知回调签名（main 注入 notify 包 Dispatcher 的适配器；
// upload 包不依赖 notify 以避免环）：resourceID 为 uuid.Nil 表示无关联资源。
type NotifyFunc func(userID uuid.UUID, eventType, title, body string, resourceID uuid.UUID)

// SetNotifyDispatcher 注入站内通知回调（幂等）：Complete 的完成路径
// （新文件创建/覆盖新版本成功）通知属主 upload.completed（标题含文件名），
// 隔离终态通知 upload.quarantined。回调不改变会话终态。
func (s *Service) SetNotifyDispatcher(fn NotifyFunc) {
	if fn != nil {
		s.notifyDispatcher = fn
	}
}

// notifyOwner 尽力通知（回调未注入或错误均忽略，不阻断上传终态）。
func (s *Service) notifyOwner(v UploadSession, eventType, title, body string, resourceID uuid.UUID) {
	if s.notifyDispatcher == nil {
		return
	}
	s.notifyDispatcher(v.UserID, eventType, title, body, resourceID)
}

// SetVersionTarget 注入「覆盖为新版本」能力（validate 与 replace 需成对注入，幂等）：
// validate 由 StartReplace 在创建会话时校验目标文件并取现有名称/父目录；
// replace 由 Complete 在校验/扫描通过后落库新版本。任一为 nil 时不覆盖既有配置。
func (s *Service) SetVersionTarget(validate func(uuid.UUID, uuid.UUID) (files.File, error), replace func(uuid.UUID, uuid.UUID, string, string, int64, string) (bool, error)) {
	if validate != nil && replace != nil {
		s.validateTarget, s.replaceFile = validate, replace
	}
}

// MaxSize 返回单文件大小上限（tus Tus-Max-Size 响应头数据源）：
// 经 SetMaxSizeProvider 注入时为热读取值（与 Start 校验保持一致），
// 读失败回退构造值。
func (s *Service) MaxSize() int64 { return s.effectiveMaxSize() }

// SetMetadata 持久化 tus 原始 Upload-Metadata 头，供 HEAD 请求回显。
func (s *Service) SetMetadata(id uuid.UUID, metadata string) error {
	v, e := s.store.Get(id)
	if e != nil {
		return e
	}
	v.Metadata = metadata
	return s.store.Update(v)
}

// Start 创建常规上传会话（Complete 后创建新文件）。
func (s *Service) Start(user, parent uuid.UUID, name string, size int64, expected string) (UploadSession, error) {
	return s.start(user, parent, name, size, expected, nil)
}

// StartReplace 创建「覆盖为新版本」会话：target 必须是 user 有 CanWrite 权限的
// 已有文件（type=file、未删除）；会话名称与父目录沿用目标文件现有值，
// 请求侧传入的文件名被忽略。Complete 成功后向该文件追加新版本并按保留策略裁剪。
func (s *Service) StartReplace(user, target uuid.UUID, size int64, expected string) (UploadSession, error) {
	if s.validateTarget == nil || s.replaceFile == nil {
		return UploadSession{}, ErrTargetUnavailable
	}
	f, e := s.validateTarget(user, target)
	if e != nil {
		return UploadSession{}, e
	}
	parent := uuid.Nil
	if f.ParentID != nil {
		parent = *f.ParentID
	}
	return s.start(user, parent, f.Name, size, expected, &f.ID)
}

// start 创建会话并计入 docflow_upload_sessions_total{status=created|failed}
// （自定义 API 与 tus 创建扩展共用入口）。
func (s *Service) start(user, parent uuid.UUID, name string, size int64, expected string, targetFileID *uuid.UUID) (UploadSession, error) {
	v, err := s.createSession(user, parent, name, size, expected, targetFileID)
	if err != nil {
		metrics.IncUploadSession(metrics.UploadSessionFailed)
	} else {
		metrics.IncUploadSession(metrics.UploadSessionCreated)
	}
	return v, err
}

func (s *Service) createSession(user, parent uuid.UUID, name string, size int64, expected string, targetFileID *uuid.UUID) (UploadSession, error) {
	if targetFileID == nil {
		n, e := files.NormalizeName(name)
		if e != nil {
			return UploadSession{}, e
		}
		name = n
		if s.validateParent != nil {
			if e = s.validateParent(user, parent); e != nil {
				return UploadSession{}, e
			}
		}
	}
	if size < 0 || size > s.effectiveMaxSize() {
		return UploadSession{}, ErrSize
	}
	expected = strings.TrimSpace(expected)
	if expected != "" {
		decoded, err := hex.DecodeString(expected)
		if err != nil || len(decoded) != sha256.Size {
			return UploadSession{}, ErrHash
		}
	}
	id := uuid.New()
	v := UploadSession{ID: id, UserID: user, ParentID: parent, TusID: id.String(), Name: name, Size: size, ExpectedSHA256: strings.ToLower(expected), Status: StatusUploading, ExpiresAt: time.Now().Add(s.ttl), CreatedAt: time.Now(), StorageKey: fmt.Sprintf("tmp/%s", id), TargetFileID: targetFileID}
	e := s.store.Save(v)
	return v, e
}

// Append 追加一段上传内容（请求体大小未知，按 remaining/patchMax 上限
// 流式截断，超出报 ErrSize/ErrPatchTooLarge）。
func (s *Service) Append(id uuid.UUID, offset int64, r io.Reader) (UploadSession, error) {
	return s.append(id, offset, -1, r)
}

// AppendWithLength 同 Append；declaredLength 为请求声明的 Content-Length
// （<0 表示未知/分块传输），用于在写入前直接拒绝超限请求体。
func (s *Service) AppendWithLength(id uuid.UUID, offset, declaredLength int64, r io.Reader) (UploadSession, error) {
	return s.append(id, offset, declaredLength, r)
}

// append 流式追加：不再 io.ReadAll 整体缓冲，countingReader 边读边写存储。
// 并发防护双保险：
//  1. 进程内 per-session 互斥串行化同会话的 Append/Complete；
//  2. store 侧 CAS（AdvanceOffset）——存储写入前预占 offset+n（n 为本次
//     可能写入的最大字节数），写入失败或超限时回退 offset-n。跨进程并发
//     （多实例部署）下预占失败即 ErrOffset，防止两个实例同时写同一 offset。
//
// 取舍：LocalStorage 无法回滚已落盘字节——本实现按「绝对 offset 覆写」
// （OffsetAppender.AppendAt）使失败/超限残留被同 offset 重试覆盖，无需
// truncate；放弃上传的 tmp/* 残留由 janitor 清理。
func (s *Service) append(id uuid.UUID, offset, declared int64, r io.Reader) (UploadSession, error) {
	unlock := s.lockSession(id)
	defer unlock()
	v, e := s.store.Get(id)
	if e != nil {
		return v, e
	}
	if time.Now().After(v.ExpiresAt) {
		return v, ErrExpired
	}
	if v.Status != StatusUploading || offset != v.Offset || offset < 0 || offset > v.Size {
		return v, ErrOffset
	}
	remaining := v.Size - v.Offset
	if declared > remaining {
		return v, ErrSize
	}
	if s.patchMax > 0 && declared > s.patchMax {
		return v, ErrPatchTooLarge
	}
	// 本次写入上限：会话剩余容量与单请求上限取小；声明长度已知且更小时取声明值。
	limit := remaining
	if s.patchMax > 0 && s.patchMax < limit {
		limit = s.patchMax
	}
	if declared >= 0 && declared < limit {
		limit = declared
	}
	caser, hasCaser := s.store.(offsetCaser)
	reserved := int64(0)
	if hasCaser {
		if e := caser.AdvanceOffset(id, offset, limit, time.Now()); e != nil {
			// offset/状态已被并发改写（跨进程 Append/Complete/janitor）。
			return v, ErrOffset
		}
		reserved = limit
	}
	// 流式复制：hard cap = limit，countingReader 记录实际字节数。
	cr := &countingReader{r: r}
	bounded := io.LimitReader(cr, limit)
	var n int64
	if oa, ok := s.storage.(OffsetAppender); ok {
		n, e = oa.AppendAt(v.StorageKey, offset, bounded)
	} else {
		n, e = s.storage.Append(v.StorageKey, bounded)
	}
	// 溢出检测：写满上限后源仍有剩余字节 → 本次 PATCH 超出容量/单请求上限。
	if e == nil && n == limit {
		if extra, _ := cr.Read(make([]byte, 1)); extra > 0 {
			e = ErrSize
			if s.patchMax > 0 && declared < 0 && limit == s.patchMax && remaining > limit {
				e = ErrPatchTooLarge
			}
		}
	}
	if e != nil {
		// 回退 CAS 预占（offset 复原）；已写存储字节不回滚——同 offset 重试
		// 覆写（AppendAt），放弃上传由 janitor 清理 tmp/*。
		if hasCaser && reserved > 0 {
			_ = caser.AdvanceOffset(id, offset+reserved, -reserved, time.Now())
		}
		return v, e
	}
	v.Offset = offset + n
	if hasCaser {
		if n != reserved {
			// 实际写入与预占不符（客户端提前断流等）：把 offset 收敛到实际值。
			if e := caser.AdvanceOffset(id, offset+reserved, n-reserved, time.Now()); e != nil {
				return v, e
			}
		}
	} else if e := s.store.Update(v); e != nil {
		return v, e
	}
	return v, nil
}
func (s *Service) Complete(id uuid.UUID) (UploadSession, error) {
	unlock := s.lockSession(id)
	defer unlock()
	v, e := s.store.Get(id)
	if e != nil {
		return v, e
	}
	// 终态幂等：available/failed/quarantined 直接返回当前状态，不重复处理。
	switch v.Status {
	case StatusAvailable:
		return v, nil
	case StatusQuarantined:
		return v, ErrRejected
	case StatusFailed:
		return v, ErrFailed
	}
	if v.Offset != v.Size {
		return v, ErrSize
	}
	// uploading → verifying CAS：tus 自动补完与手动 complete 并发时仅一方
	// 推进流水线；竞争失败方按重读到的状态返回幂等/终态/冲突语义。
	marker, hasMarker := s.store.(stateMarker)
	if hasMarker {
		won, e := marker.MarkVerifying(id, v.Size)
		if e != nil {
			return v, e
		}
		if !won {
			return s.resolveCompletionRace(id, v)
		}
	} else {
		v.Status = StatusVerifying
		if e = s.store.Update(v); e != nil {
			return v, e
		}
	}
	v.Status = StatusVerifying
	// verify 阶段：读取 + SHA-256 全量校验（计入
	// docflow_upload_processing_duration_seconds{stage=verify}）。
	verifyStart := time.Now()
	r, e := s.storage.Read(v.StorageKey)
	if e != nil {
		return v, e
	}
	h := sha256.New()
	n, e := io.Copy(h, r)
	r.Close()
	if e != nil {
		return v, e
	}
	metrics.ObserveUploadProcessing(metrics.UploadStageVerify, time.Since(verifyStart).Seconds())
	if n != v.Size || (v.ExpectedSHA256 != "" && !strings.EqualFold(fmt.Sprintf("%x", h.Sum(nil)), v.ExpectedSHA256)) {
		v.Status = StatusFailed
		_ = s.store.Update(v)
		return v, ErrChecksum
	}
	// verifying → scanning 条件迁移：中途被 janitor 置 failed 等情况下中止。
	if hasMarker {
		won, e := marker.MarkScanning(id)
		if e != nil {
			return v, e
		}
		if !won {
			return s.resolveCompletionRace(id, v)
		}
	} else {
		v.Status = StatusScanning
		if e = s.store.Update(v); e != nil {
			return v, e
		}
	}
	v.Status = StatusScanning
	r, e = s.storage.Read(v.StorageKey)
	if e != nil {
		v.Status = StatusFailed
		_ = s.store.Update(v)
		return v, e
	}
	e = s.scanner.Scan(r)
	r.Close()
	if e != nil {
		v.Status = StatusQuarantined
		if updateErr := s.store.Update(v); updateErr != nil {
			return v, updateErr
		}
		// 隔离终态通知属主（尽力而为；未落成文件，resource 为空）。
		s.notifyOwner(v, "upload.quarantined", "上传已隔离："+v.Name, "文件「"+v.Name+"」未通过安全扫描，已被隔离；请检查文件内容后重新上传。", uuid.Nil)
		return v, ErrRejected
	}
	finalKey := fmt.Sprintf("objects/%s/%s", v.UserID, v.ID)
	r, e = s.storage.Read(v.StorageKey)
	if e == nil {
		e = s.storage.Put(finalKey, r)
		if closeErr := r.Close(); e == nil {
			e = closeErr
		}
	}
	if e != nil {
		v.Status = StatusFailed
		_ = s.store.Update(v)
		return v, e
	}
	if e = s.storage.Delete(v.StorageKey); e != nil {
		v.Status = StatusFailed
		_ = s.store.Update(v)
		return v, e
	}
	v.StorageKey = finalKey
	sum := fmt.Sprintf("%x", h.Sum(nil))
	if v.TargetFileID != nil {
		// 覆盖为新版本：向目标文件追加版本并按保留策略裁剪，不创建新 File。
		if s.replaceFile == nil {
			v.Status = StatusFailed
			_ = s.store.Update(v)
			return v, ErrTargetUnavailable
		}
		newBlob, re := s.replaceFile(v.UserID, *v.TargetFileID, v.StorageKey, sum, v.Size, "application/octet-stream")
		if re != nil {
			v.Status = StatusFailed
			_ = s.store.Update(v)
			return v, re
		}
		if !newBlob {
			// 同 sha256 的 available blob 已存在（内容去重）：本次上传的物理对象冗余，
			// 尽力清理；失败不影响会话终态（孤儿对象不产生引用，由存储巡检兜底）。
			_ = s.storage.Delete(finalKey)
		}
		// 完成路径通知属主（覆盖新版本成功，资源为目标文件）。
		s.notifyOwner(v, "upload.completed", "上传完成："+v.Name, "文件「"+v.Name+"」已作为新版本写入，校验与安全扫描通过。", *v.TargetFileID)
		if s.fileComplete != nil {
			s.fileComplete(*v.TargetFileID)
		}
	} else if s.createFile != nil {
		fileID, newBlob, ce := s.createFile(v.UserID, v.ParentID, v.Name, v.StorageKey, v.Size, sum, "application/octet-stream")
		if ce != nil {
			v.Status = StatusFailed
			_ = s.store.Update(v)
			return v, ce
		}
		if !newBlob {
			// 内容去重复用既有 blob：本次上传的 finalKey 物理对象冗余，
			// 尽力清理（与 replace 分支同策略；失败由存储巡检兜底）。
			_ = s.storage.Delete(finalKey)
		}
		// 完成路径通知属主（新文件创建成功，资源为新建文件）。
		s.notifyOwner(v, "upload.completed", "上传完成："+v.Name, "文件「"+v.Name+"」已完成校验与安全扫描，可以下载或预览。", fileID)
		if s.fileComplete != nil {
			s.fileComplete(fileID)
		}
	}
	now := time.Now()
	v.Status = StatusAvailable
	v.CompletedAt = &now
	// scanning → available 条件终态写入（同时落库终态 storage_key）：被并发
	// 流转（如 janitor 置 failed）时不覆盖，按重读状态返回。
	if hasMarker {
		won, e := marker.MarkAvailable(id, v.StorageKey, now)
		if e != nil {
			return v, e
		}
		if !won {
			return s.resolveCompletionRace(id, v)
		}
	} else if e = s.store.Update(v); e != nil {
		return v, e
	}
	return v, nil
}

// resolveCompletionRace 在 CAS 状态迁移竞争失败后重读会话并映射语义：
// available 幂等返回；quarantined/failed 返回对应终态错误；verifying/
// scanning 表示另一方补完仍在进行（ErrInProgress，可重试/轮询）；
// 仍为 uploading 说明 offset 已被并发 Append 改变（不符 size），返回 ErrSize。
func (s *Service) resolveCompletionRace(id uuid.UUID, stale UploadSession) (UploadSession, error) {
	cur, err := s.store.Get(id)
	if err != nil {
		return stale, err
	}
	switch cur.Status {
	case StatusAvailable:
		return cur, nil
	case StatusQuarantined:
		return cur, ErrRejected
	case StatusFailed:
		return cur, ErrFailed
	case StatusVerifying, StatusScanning:
		return cur, ErrInProgress
	default:
		return cur, ErrSize
	}
}
func (s *Service) Get(id uuid.UUID) (UploadSession, error) { return s.store.Get(id) }
