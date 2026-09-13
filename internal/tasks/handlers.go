package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/upload"
)

// errUnknownTaskType 表示载荷的任务类型未注册（不应发生，防御性返回）。
func errUnknownTaskType(taskType string) error {
	return fmt.Errorf("unknown task type %q", taskType)
}

// UploadCompleter 抽象上传补完能力（*upload.Service 满足）。
// Complete 对 available/quarantined/failed 等终态幂等，重复投递安全。
type UploadCompleter interface {
	Complete(id uuid.UUID) (upload.UploadSession, error)
}

// WebpkgExtractor 抽象网页包自动解包能力（*webpkg.Service 满足）。
// AutoExtract 内建 WEBPKG_ENABLED 开关与 zip 候选判定，通过后调用
// ExtractForFile 执行解包（幂等重建，失败置 blocked/failed 不影响文件）。
type WebpkgExtractor interface {
	AutoExtract(fileID uuid.UUID)
}

// CompleteUploadHandler 返回「上传补完」处理函数：调 upload.Service.Complete
// （verify→scan→落库），成功后记录审计（upload.complete；覆盖为新版本的会话
// 另记 version.create）——与原 tus PATCH 内联 goroutine 行为一致。
// 终态错误（校验失败/隔离/未找到等）已内建幂等，归零返回不重试；
// 其余（存储/DB 等瞬时错误）返回 error 交由 redis 驱动重试。
func CompleteUploadHandler(completions UploadCompleter, recorder audit.Recorder) TaskFunc {
	if recorder == nil {
		recorder = audit.NopRecorder{}
	}
	return func(ctx context.Context, sessionID uuid.UUID) error {
		v, err := completions.Complete(sessionID)
		if err != nil {
			if isPermanentCompleteError(err) {
				log.Printf("[tasks] complete-upload %s terminal state: %v", sessionID, err)
				return nil
			}
			return err
		}
		owner := v.UserID
		_ = recorder.Record(audit.Entry{UserID: &owner, Action: audit.ActionUploadComplete, ResourceType: audit.ResourceUpload, ResourceID: v.ID.String(), Status: audit.StatusSuccess, Metadata: `{"name":"` + sanitizeAuditToken(v.Name) + `","size":` + strconv.FormatInt(v.Size, 10) + `}`})
		// 覆盖为新版本的会话：另记一条 version.create（资源为目标文件）。
		if v.TargetFileID != nil {
			_ = recorder.Record(audit.Entry{UserID: &owner, Action: audit.ActionVersionCreate, ResourceType: audit.ResourceFile, ResourceID: v.TargetFileID.String(), Status: audit.StatusSuccess, Metadata: `{"upload_id":"` + v.ID.String() + `","size":` + strconv.FormatInt(v.Size, 10) + `}`})
		}
		return nil
	}
}

// isPermanentCompleteError 判定 Complete 错误是否为终态（重试必然同样
// 失败）：会话不存在、大小/校验和不符、隔离、失败、版本目标不可用。
func isPermanentCompleteError(err error) bool {
	return errors.Is(err, upload.ErrNotFound) ||
		errors.Is(err, upload.ErrSize) ||
		errors.Is(err, upload.ErrChecksum) ||
		errors.Is(err, upload.ErrRejected) ||
		errors.Is(err, upload.ErrFailed) ||
		errors.Is(err, upload.ErrTargetUnavailable)
}

// ExtractWebpkgHandler 返回「网页包自动解包」处理函数：经 AutoExtract
// （开关与 zip 候选判定内建）调用 webpkg.Service.ExtractForFile；解包失败
// 置 blocked/failed 并记日志，不影响文件可用性（与原上传完成钩子一致，
// 因此恒返回 nil 不触发重试）。
func ExtractWebpkgHandler(extractor WebpkgExtractor) TaskFunc {
	return func(ctx context.Context, fileID uuid.UUID) error {
		if extractor == nil {
			return nil
		}
		extractor.AutoExtract(fileID)
		return nil
	}
}

// sanitizeAuditToken 只保留可安全嵌入 JSON 字符串的字符，避免审计日志注入
// （与 internal/http 同名实现保持一致的策略）。
func sanitizeAuditToken(value string) string {
	var b []byte
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '@', ch == '.', ch == '-', ch == '_':
			b = append(b, ch)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}
