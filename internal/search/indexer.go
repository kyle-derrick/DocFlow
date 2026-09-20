package search

import (
	"errors"
	"io"
	"log"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
)

// Indexer 在文件版本落库后重建其检索索引：查文件行与当前版本 blob →
// 文本判定（mime/扩展名白名单）→ 经 Storage.Read 限 2MB 抽取内容 →
// Store.IndexFile upsert。由 task:search-index 队列异步触发（tasks 包的
// SearchIndexHandler），失败仅记日志不影响上传主流程；重复投递幂等
// （整行覆盖）。
type Indexer struct {
	store   *Store
	db      *gorm.DB
	storage upload.Storage
}

func NewIndexer(store *Store, db *gorm.DB, storage upload.Storage) *Indexer {
	return &Indexer{store: store, db: db, storage: storage}
}

// Index 重建 fileID 的索引（task:search-index 处理入口）：
//   - 文件行已不存在（硬删竞态）：RemoveFile 双保险（正常路径由外键级联）；
//   - 软删 / 目录：不更新索引（查询侧 JOIN 已排除软删；目录不入索引）；
//   - 无当前版本：仅名称索引（上传落库必有 v1，此为防御分支）；
//   - 内容读取失败：降级仅名称（best-effort，错误记日志）。
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
	if f.CurrentVersionID != nil {
		versionID = *f.CurrentVersionID
		var version files.FileVersion
		if err := ix.db.Where("id = ?", versionID).First(&version).Error; err == nil {
			var blob files.ObjectBlob
			if err := ix.db.Where("id = ?", version.ObjectBlobID).First(&blob).Error; err == nil {
				c, rerr := ReadContent(ix.storage, blob, f.Name)
				if rerr != nil {
					log.Printf("[search] read content for %s: %v (index name only)", fileID, rerr)
				} else {
					content = c
				}
			}
		}
	}
	spaceID := f.SpaceID
	return ix.store.IndexFile(fileID, versionID, f.OwnerID, &spaceID, f.Name, content)
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
