package search

import (
	"context"
	"errors"
	"io"
	"log"
	"time"

	"github.com/google/uuid"
	gorm "gorm.io/gorm"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
)

// Indexer 在文件版本落库后重建其检索索引：查文件行与当前版本 blob →
// 文本判定（mime/扩展名白名单）→ 经 Storage.Read 限 2MB 抽取内容 →
// Store.IndexFile upsert。由 task:search-index 队列异步触发（tasks 包的
// SearchIndexHandler），失败仅记日志不影响上传主流程；重复投递幂等
// （整行覆盖）。
type VectorIndexer interface {
	IndexVector(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID, string, string) error
}

// OCRExtractor 抽象图片文字提取（*ai.Service 经 SetOCR 注入；接口放
// search 包避免 ai→search 循环依赖的反向）。
type OCRExtractor interface {
	ExtractImageText(ctx context.Context, storageKey, mimeType, name string, size int64) (string, error)
}

// ocrTimeout 单次图片 OCR 的等待上限：视觉模型对大图较慢，90s 覆盖常规
// 响应；超时/失败均降级仅名称索引，不拖垮 task:search-index 主流程。
const ocrTimeout = 90 * time.Second

type Indexer struct {
	store   *Store
	db      *gorm.DB
	storage upload.Storage
	vector  VectorIndexer
	ocr     OCRExtractor
}

func (ix *Indexer) SetVectorIndexer(v VectorIndexer) { ix.vector = v }

// SetOCR 注入图片文字提取器（nil = 不做 OCR，行为与旧版一致）；提取器
// 内部对 OCR 关闭/超限安静返回空，注入后无需按配置开关装卸。
func (ix *Indexer) SetOCR(o OCRExtractor) { ix.ocr = o }

func NewIndexer(store *Store, db *gorm.DB, storage upload.Storage) *Indexer {
	return &Indexer{store: store, db: db, storage: storage}
}

// Index 重建 fileID 的索引（task:search-index 处理入口）：
//   - 文件行已不存在（硬删竞态）：RemoveFile 双保险（正常路径由外键级联）；
//   - 软删 / 目录：不更新索引（查询侧 JOIN 已排除软删；目录不入索引）；
//   - 无当前版本：仅名称索引（上传落库必有 v1，此为防御分支）；
//   - 内容读取失败：降级仅名称（best-effort，错误记日志）；
//   - 图片 OCR：文本抽取为空且为可 OCR 图片时经注入提取器补全（失败
//     降级仅名称，错误记日志）。
func (ix *Indexer) Index(fileID uuid.UUID) error {
	var f files.File
	err := ix.db.Where("id = ?", fileID).First(&f).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ix.store.RemoveFile(fileID)
		}
		return err
	}
	if f.DeletedAt != nil || f.Type != "file" {
		return nil
	}
	versionID := uuid.Nil
	var content string
	// blob 提升到函数级作用域：图片 OCR 兜底分支需要 mime/大小/storage
	// key；版本或 blob 行缺失时 blobFound=false，跳过 OCR（仅名称索引）。
	var blob files.ObjectBlob
	blobFound := false
	if f.CurrentVersionID != nil {
		versionID = *f.CurrentVersionID
		var version files.FileVersion
		if err := ix.db.Where("id = ?", versionID).First(&version).Error; err == nil {
			if err := ix.db.Where("id = ?", version.ObjectBlobID).First(&blob).Error; err == nil {
				blobFound = true
				c, rerr := ReadContent(ix.storage, blob, f.Name)
				if rerr != nil {
					log.Printf("[search] read content for %s: %v (index name only)", fileID, rerr)
				} else {
					content = c
				}
			}
		}
	}
	// 图片 OCR 兜底：文本抽取为空（图片非文本类型）且为可 OCR 图片时补全
	// 内容；未注入提取器/失败返回空（降级仅名称，不阻塞索引写入）。
	if content == "" && blobFound {
		if text := ix.ocrText(fileID, blob, f.Name); text != "" {
			content = text
		}
	}
	spaceID := f.SpaceID
	if err := ix.store.IndexFile(fileID, versionID, f.OwnerID, &spaceID, f.Name, content); err != nil {
		return err
	}
	if ix.vector != nil && content != "" {
		if err := ix.vector.IndexVector(context.Background(), fileID, versionID, &spaceID, f.Name, content); err != nil {
			return err
		}
	}
	return nil
}

// ocrText 对文本抽取为空的 blob 尝试 OCR 补全内容：仅当已注入提取器且
// 为可 OCR 图片时发起（90s 上限）；失败记日志返回空串（索引回退仅名称，
// 提取文本为空同样视为无内容）。
func (ix *Indexer) ocrText(fileID uuid.UUID, blob files.ObjectBlob, name string) string {
	if ix.ocr == nil || !IsImageIndexable(blob.MimeType, name) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), ocrTimeout)
	defer cancel()
	text, err := ix.ocr.ExtractImageText(ctx, blob.StorageKey, blob.MimeType, name, blob.Size)
	if err != nil {
		log.Printf("[search] ocr for %s: %v (index name only)", fileID, err)
		return ""
	}
	return text
}

// ReadContent 读取 blob 的文本内容：大小超上限（2MB）或非文本 mime/扩展名
// 返回空串（仅名称索引）；否则经 Storage.Read 限流读取全部内容。
func ReadContent(storage upload.Storage, blob files.ObjectBlob, name string) (string, error) {
	if blob.Size <= 0 || blob.Size > MaxContentBytes {
		return "", nil
	}
	if !IsTextIndexable(blob.MimeType, name) {
		return "", nil
	}
	r, err := storage.Read(blob.StorageKey)
	if err != nil {
		return "", err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, MaxContentBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
