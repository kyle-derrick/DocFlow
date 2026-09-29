// studio.go：Studio 项目注册表（studio_projects 表，migration 051）。
//
// 项目 = 空间 + 根目录 + 执行引擎的绑定，纯属主维度的轻量记录（不含文件本
// 体——目录/文件仍在空间体系内）。此前项目清单仅存浏览器 localStorage，换
// 浏览器/清存储即丢入口；服务端化后随账号走。API：GET/POST/PUT/DELETE
// /api/v1/studio/projects（internal/http/studio.go 薄 handler）。
package studio

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// 执行引擎取值（CHECK 约束同 migration 051）：platform=平台直读写（默认）；
// docker=Docker 沙箱（Agent 创作舱任务）。
const (
	EnginePlatform = "platform"
	EngineDocker   = "docker"
)

// 名称 / harness / model 的长度上限（rune 计；与 DB CHECK 一致，服务层先拦）。
const (
	MaxNameRunes    = 100
	MaxHarnessRunes = 32
	MaxModelRunes   = 160
)

// ErrInvalidProject 表示项目载荷非法（HTTP 400 语义）。
var ErrInvalidProject = errors.New("invalid studio project")

// Project 对应 studio_projects 一行（本人维度）。SpaceName/FolderPath 为
// 创建时快照（展示用弱引用，不追随空间重命名）；Harness/Model 空串 = 跟随
// 平台默认。
type Project struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID     uuid.UUID `gorm:"type:uuid;not null;index" json:"-"`
	Name       string    `gorm:"not null" json:"name"`
	SpaceID    uuid.UUID `gorm:"type:uuid;not null" json:"space_id"`
	RootFolder uuid.UUID `gorm:"type:uuid;not null;column:root_folder_id" json:"root_folder_id"`
	SpaceName  string    `gorm:"not null;default:''" json:"space_name"`
	FolderPath string    `gorm:"not null;default:''" json:"folder_path"`
	Engine     string    `gorm:"not null;default:'platform'" json:"engine"`
	Harness    string    `gorm:"not null;default:''" json:"harness,omitempty"`
	Model      string    `gorm:"not null;default:''" json:"model,omitempty"`
	CreatedAt  time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt  time.Time `gorm:"not null" json:"updated_at"`
}

// TableName 显式映射到 studio_projects。
func (Project) TableName() string { return "studio_projects" }

// normalizeEngine 归一引擎取值：空 = platform 默认。
func normalizeEngine(engine string) string {
	if engine == EngineDocker {
		return EngineDocker
	}
	return EnginePlatform
}

// ValidateProject 校验载荷（创建/更新共用）：name 去空白后 1-100 字符；
// space/folder 必填 uuid；engine 白名单（空归一 platform）；harness/model
// 长度上限。
func ValidateProject(p *Project) error {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return fmt.Errorf("%w: name 不能为空", ErrInvalidProject)
	}
	if len([]rune(name)) > MaxNameRunes {
		return fmt.Errorf("%w: name 过长（≤%d 字符）", ErrInvalidProject, MaxNameRunes)
	}
	p.Name = name
	if p.SpaceID == uuid.Nil {
		return fmt.Errorf("%w: space_id 不能为空", ErrInvalidProject)
	}
	if p.RootFolder == uuid.Nil {
		return fmt.Errorf("%w: root_folder_id 不能为空", ErrInvalidProject)
	}
	p.Engine = normalizeEngine(p.Engine)
	if len([]rune(p.Harness)) > MaxHarnessRunes {
		return fmt.Errorf("%w: harness 过长（≤%d 字符）", ErrInvalidProject, MaxHarnessRunes)
	}
	if len([]rune(p.Model)) > MaxModelRunes {
		return fmt.Errorf("%w: model 过长（≤%d 字符）", ErrInvalidProject, MaxModelRunes)
	}
	p.SpaceName = strings.TrimSpace(p.SpaceName)
	p.FolderPath = strings.TrimSpace(p.FolderPath)
	return nil
}

// Store 为 studio_projects 的 gorm 存储（属主维度读写）。
type Store struct{ db *gorm.DB }

// NewStore 构造存储。
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// List 返回用户全部项目（updated_at 倒序，最近使用/新建在前）。
func (s *Store) List(user uuid.UUID) ([]Project, error) {
	var rows []Project
	err := s.db.Where("user_id = ?", user).Order("updated_at DESC").Find(&rows).Error
	return rows, err
}

// Create 新建项目（服务端生成 id；载荷先经 ValidateProject）。
func (s *Store) Create(user uuid.UUID, p Project) (Project, error) {
	if err := ValidateProject(&p); err != nil {
		return Project{}, err
	}
	p.ID = uuid.Must(uuid.NewV7())
	p.UserID = user
	if err := s.db.Create(&p).Error; err != nil {
		return Project{}, err
	}
	return p, nil
}

// Update 覆盖式更新属主项目（载荷先经 ValidateProject；改绑 space/folder 等
// 全量字段由调用方提交）。不存在/非属主返回 found=false（HTTP 404 语义——
// 不区分两者，避免探测他人项目 id）。
func (s *Store) Update(user, id uuid.UUID, p Project) (Project, bool, error) {
	if err := ValidateProject(&p); err != nil {
		return Project{}, false, err
	}
	var row Project
	err := s.db.Where("id = ? AND user_id = ?", id, user).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, err
	}
	row.Name, row.SpaceID, row.RootFolder = p.Name, p.SpaceID, p.RootFolder
	row.SpaceName, row.FolderPath, row.Engine = p.SpaceName, p.FolderPath, p.Engine
	row.Harness, row.Model = p.Harness, p.Model
	if err := s.db.Save(&row).Error; err != nil {
		return Project{}, false, err
	}
	return row, true, nil
}

// Delete 删除属主项目（不存在/非属主返回 false；文件本体不受影响）。
func (s *Store) Delete(user, id uuid.UUID) (bool, error) {
	res := s.db.Where("id = ? AND user_id = ?", id, user).Delete(&Project{})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
