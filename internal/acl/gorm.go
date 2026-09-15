package acl

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/files"
)

var _ Repo = (*GormRepo)(nil)

// GormRepo 是 Repo 的 GORM/PostgreSQL 实现（folder_acl 表见 migration 031）。
// permissions text[] 经 ::text 字面量扫描/绑定（避免驱动数组类型映射差异）；
// 元素恒为固定权限词，字面量不含需转义字符。
type GormRepo struct{ db *gorm.DB }

func NewGormRepo(db *gorm.DB) *GormRepo { return &GormRepo{db: db} }

// entryRow 为 folder_acl 的扫描行（permissions 为 text[] 的字面量形式）。
type entryRow struct {
	ID          uuid.UUID `gorm:"column:id"`
	FolderID    uuid.UUID `gorm:"column:folder_id"`
	SubjectType string    `gorm:"column:subject_type"`
	SubjectID   uuid.UUID `gorm:"column:subject_id"`
	Effect      string    `gorm:"column:effect"`
	Permissions string    `gorm:"column:permissions"`
	CreatedBy   uuid.UUID `gorm:"column:created_by"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (r entryRow) toEntry() Entry {
	return Entry{
		ID: r.ID, FolderID: r.FolderID, SubjectType: r.SubjectType, SubjectID: r.SubjectID,
		Effect: r.Effect, Permissions: parseTextArray(r.Permissions),
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
	}
}

// parseTextArray 解析 PostgreSQL text[] 的字面量形式（{"read","write"}）。
// 元素为固定权限词，不处理转义序列。
func parseTextArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "NULL" || raw == "{}" {
		return nil
	}
	raw = strings.TrimPrefix(raw, "{")
	raw = strings.TrimSuffix(raw, "}")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		out = append(out, p)
	}
	return out
}

// formatTextArray 生成 text[] 字面量（元素为固定权限词，无需转义）。
func formatTextArray(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = `"` + v + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

// Folder 返回未删除目录行；不存在（或为文件/软删）返回 ErrNotFound。
func (g *GormRepo) Folder(id uuid.UUID) (files.File, error) {
	var f files.File
	err := g.db.Where("id = ? AND type = 'folder' AND deleted_at IS NULL", id).First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return files.File{}, ErrNotFound
	}
	return f, err
}

// ListByFolder 返回目录全部条目（created_at, id 稳定排序）。
func (g *GormRepo) ListByFolder(folderID uuid.UUID) ([]Entry, error) {
	var rows []entryRow
	err := g.db.Raw(`SELECT id, folder_id, subject_type, subject_id, effect,
		permissions::text AS permissions, created_by, created_at
		FROM folder_acl WHERE folder_id = ? ORDER BY created_at, id`, folderID).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toEntry())
	}
	return out, nil
}

// Replace 整体替换目录条目：事务内 DELETE 全部旧行后批量 INSERT 新行。
func (g *GormRepo) Replace(folderID uuid.UUID, entries []Entry) error {
	return g.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM folder_acl WHERE folder_id = ?", folderID).Error; err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		values := make([]string, 0, len(entries))
		args := make([]any, 0, len(entries)*8)
		for _, e := range entries {
			values = append(values, "(?, ?, ?, ?, ?, ?::text[], ?, ?)")
			args = append(args, e.ID, e.FolderID, e.SubjectType, e.SubjectID, e.Effect,
				formatTextArray(e.Permissions), e.CreatedBy, e.CreatedAt)
		}
		return tx.Exec(`INSERT INTO folder_acl
			(id, folder_id, subject_type, subject_id, effect, permissions, created_by, created_at)
			VALUES `+strings.Join(values, ", "), args...).Error
	})
}

// maxChainDepth 链向上遍历的步数上限（folder.max_depth 默认 32 的防御性
// 上界，防 parent 环导致死循环）。
const maxChainDepth = 64

// ChainForFile 沿 parent 链自 file/folder 向上到团队根收集条目（由近及远）：
//   - 目标为文件：链自其父目录开始（文件自身不挂条目）；无父目录返回空链；
//   - 目标为目录：链含该目录自身；
//   - 途中目录缺失/软删（断链）：截断已收集部分（best-effort，不失败）；
//   - 到团队根（is_root）或无父目录终止。
func (g *GormRepo) ChainForFile(fileOrFolderID uuid.UUID) ([]ChainNode, error) {
	var start files.File
	err := g.db.Select("id", "parent_id", "type").
		Where("id = ? AND deleted_at IS NULL", fileOrFolderID).First(&start).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cur := fileOrFolderID
	if start.Type != "folder" {
		if start.ParentID == nil {
			return nil, nil
		}
		cur = *start.ParentID
	}
	var chain []ChainNode
	for depth := 0; depth < maxChainDepth; depth++ {
		var f files.File
		err := g.db.Select("id", "parent_id", "is_root").
			Where("id = ? AND type = 'folder' AND deleted_at IS NULL", cur).First(&f).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		entries, err := g.ListByFolder(f.ID)
		if err != nil {
			return nil, err
		}
		chain = append(chain, ChainNode{FolderID: f.ID, Entries: entries})
		if f.IsRoot || f.ParentID == nil {
			break
		}
		cur = *f.ParentID
	}
	return chain, nil
}
