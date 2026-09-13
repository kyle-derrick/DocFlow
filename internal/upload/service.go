package upload

import (
	"bytes"
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
)

type sessionStore interface {
	Save(UploadSession) error
	Get(uuid.UUID) (UploadSession, error)
	Update(UploadSession) error
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

type Service struct {
	store          sessionStore
	storage        Storage
	ttl            time.Duration
	maxSize        int64
	scanner        Scanner
	validateParent func(uuid.UUID, uuid.UUID) error
	// createFile 落库上传完成的新文件，返回新建文件 ID（供完成钩子等使用）。
	createFile func(uuid.UUID, uuid.UUID, string, string, int64, string, string) (uuid.UUID, error)
	// fileComplete 在文件落库成功（新文件或覆盖新版本）后同步回调一次，
	// 供网页包自动解包等后置处理注入；回调不改变会话终态（错误由注入方自负）。
	fileComplete func(uuid.UUID)
	// validateTarget 校验「覆盖为新版本」目标并返回文件行（type=file、未删除、
	// user 有 CanWrite 权限），由 files 包注入；会话沿用其现有名称与父目录。
	validateTarget func(uuid.UUID, uuid.UUID) (files.File, error)
	// replaceFile 在 Complete 成功时向目标文件追加新版本（AddVersion+Prune），
	// 返回是否新建了 blob（false 表示命中去重复用，新物理对象冗余应删除）。
	replaceFile func(uuid.UUID, uuid.UUID, string, string, int64, string) (bool, error)
}

func NewService(store sessionStore, storage Storage, ttl time.Duration, maxSize int64, scanEnabled bool, validateParent func(uuid.UUID, uuid.UUID) error, createFile func(uuid.UUID, uuid.UUID, string, string, int64, string, string) (uuid.UUID, error)) *Service {
	var scanner Scanner = AllowScanner{}
	if scanEnabled {
		scanner = RejectScanner{}
	}
	return &Service{store: store, storage: storage, ttl: ttl, maxSize: maxSize, scanner: scanner, validateParent: validateParent, createFile: createFile}
}
func (s *Service) SetScanner(scanner Scanner) {
	if scanner != nil {
		s.scanner = scanner
	}
}

// SetFileCompleteHook 注入「文件落库完成」回调（新文件与覆盖新版本均触发，
// 参数为文件 ID）；注入的回调自行决定同步/异步执行策略。
func (s *Service) SetFileCompleteHook(fn func(fileID uuid.UUID)) {
	if fn != nil {
		s.fileComplete = fn
	}
}

// SetVersionTarget 注入「覆盖为新版本」能力（validate 与 replace 需成对注入，幂等）：
// validate 由 StartReplace 在创建会话时校验目标文件并取现有名称/父目录；
// replace 由 Complete 在校验/扫描通过后落库新版本。任一为 nil 时不覆盖既有配置。
func (s *Service) SetVersionTarget(validate func(uuid.UUID, uuid.UUID) (files.File, error), replace func(uuid.UUID, uuid.UUID, string, string, int64, string) (bool, error)) {
	if validate != nil && replace != nil {
		s.validateTarget, s.replaceFile = validate, replace
	}
}

// MaxSize 返回配置的单文件大小上限（tus Tus-Max-Size 响应头数据源）。
func (s *Service) MaxSize() int64 { return s.maxSize }

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
	if size < 0 || size > s.maxSize {
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

func (s *Service) Append(id uuid.UUID, offset int64, r io.Reader) (UploadSession, error) {
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
	body, e := io.ReadAll(io.LimitReader(r, remaining+1))
	if e != nil {
		return v, e
	}
	if int64(len(body)) > remaining {
		return v, ErrSize
	}
	n, e := s.storage.Append(v.StorageKey, bytes.NewReader(body))
	if e != nil {
		return v, e
	}
	v.Offset += n
	e = s.store.Update(v)
	return v, e
}
func (s *Service) Complete(id uuid.UUID) (UploadSession, error) {
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
	v.Status = StatusVerifying
	if e = s.store.Update(v); e != nil {
		return v, e
	}
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
	v.Status = StatusScanning
	if e = s.store.Update(v); e != nil {
		return v, e
	}
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
		if s.fileComplete != nil {
			s.fileComplete(*v.TargetFileID)
		}
	} else if s.createFile != nil {
		fileID, ce := s.createFile(v.UserID, v.ParentID, v.Name, v.StorageKey, v.Size, sum, "application/octet-stream")
		if ce != nil {
			v.Status = StatusFailed
			_ = s.store.Update(v)
			return v, ce
		}
		if s.fileComplete != nil {
			s.fileComplete(fileID)
		}
	}
	v.Status = StatusAvailable
	now := time.Now()
	v.CompletedAt = &now
	if e = s.store.Update(v); e != nil {
		return v, e
	}
	return v, nil
}
func (s *Service) Get(id uuid.UUID) (UploadSession, error) { return s.store.Get(id) }
