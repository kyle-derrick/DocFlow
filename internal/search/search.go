// Package search 提供文件全文搜索（文件名 + 文本内容）：
//   - 索引构建：文件版本创建（上传完成：新建 / 覆盖新版本）后经
//     task:search-index 队列异步触发（接线见 cmd/server 的 fileComplete
//     钩子），Indexer 读当前版本 blob 内容（限 2MB、文本 mime/扩展名判定）
//     后经 Repo.UpsertDoc 落 file_search_docs（超出上限或二进制仅名称）；
//   - 查询：名称 ILIKE %q%（大小写不敏感、escape %_\）OR tsv @@
//     plainto_tsquery('simple', q)，访问控制复用统一空间模型的判定
//     （owner=me OR 空间在册成员：直接成员/经用户组），软删文件排除；
//   - 清理：files 行硬删除经外键 ON DELETE CASCADE 级联移除索引行，
//     janitor 另有孤儿兜底清理（file_search_docs 无对应 files 行）。
//
// 中文检索限制：'simple' 分词器按空白切分，中文整句成为单 token，内容
// tsvector 检索对中文基本无效（英文有效）；中文场景以名称 ILIKE 兜底为主
// （ILIKE 对任意子串大小写不敏感匹配，中英文均有效）。
//
// v2 规划以 Meilisearch 替换：替换边界为 Repo 接口（UpsertDoc/RemoveDoc/
// QueryDocs）与 Indexer 的内容抽取（IsTextIndexable / ReadContent），实现
// MeilisearchRepo 后经 Store 同一入口切换；HTTP（/api/v1/search）与
// task:search-index 队列接线不感知底层引擎。
package search

import (
	"time"

	"github.com/google/uuid"
)

// Doc 一条检索文档（file_search_docs 行）：每文件一行，file_id 为主键。
type Doc struct {
	FileID    uuid.UUID
	VersionID uuid.UUID
	OwnerID   uuid.UUID
	SpaceID   *uuid.UUID
	Name      string
	// Content 文本内容；"" 表示仅名称（二进制 / 超上限 / 非文本 mime）。
	Content string
}

// Result 搜索结果条目：id/name/type/parent_id 取 files 行实时值
// （索引不复制，避免 rename 后漂移），snippet 为命中上下文。
type Result struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	ParentID  *uuid.UUID `json:"parent_id"`
	UpdatedAt time.Time  `json:"updated_at"`
	// Snippet 命中上下文：名称命中时为名称本身；内容命中时为 ts_headline
	//（'simple'，标记 [[..]]，由前端渲染高亮）；仅名称索引或未命中片段时为空。
	Snippet string `json:"snippet,omitempty"`
}

// QueryOptions 查询参数：q 为必填（HTTP 层校验非空），tag/starred 过滤可选，
// limit 缺省 20、上限 100（Store.Query 归一）。
type QueryOptions struct {
	Q       string
	TagID   *uuid.UUID
	Starred *bool
	// SpaceID 可选空间过滤（nil = 不过滤；ask_docs 的 space_id 参数）。
	SpaceID *uuid.UUID
	Limit   int
}

// QueryLimitDefault / QueryLimitMax 与 HTTP 层 /search 的 limit 约束一致。
const (
	QueryLimitDefault = 20
	QueryLimitMax     = 100
)

// Repo 抽象检索文档的数据访问；生产实现为 GormRepo（GORM/PostgreSQL，
// ILIKE + tsvector + ts_headline），测试用 MemoryRepo 验证访问控制与
// 过滤语义（模式同 share/tagging 包）。Meilisearch 替换边界即本接口。
type Repo interface {
	// UpsertDoc upsert 索引文档（按 file_id 冲突更新全部字段）。
	UpsertDoc(d Doc) error
	// RemoveDoc 删除 fileID 的索引文档（幂等）。
	RemoveDoc(fileID uuid.UUID) error
	// QueryDocs 返回 user 可读且命中 q 的结果：访问控制（owner /
	// 空间在册成员实时判定）、软删排除、tag/starred 过滤、limit 截断、
	// 名称命中优先其次 updated_at 倒序，均由实现保证。
	QueryDocs(user uuid.UUID, opts QueryOptions) ([]Result, error)
}

// Store 搜索服务：Repo 之上的薄封装（参数归一 + 对外入口），HTTP 层经
// SetSearch 注入；Indexer 经其落索引。
type Store struct {
	repo Repo
}

func NewStore(repo Repo) *Store { return &Store{repo: repo} }

// Query 检索 user 可读且命中 q 的文件（q 为空时返回空结果，HTTP 层先 400）。
func (s *Store) Query(user uuid.UUID, opts QueryOptions) ([]Result, error) {
	if opts.Q == "" {
		return []Result{}, nil
	}
	if opts.Limit <= 0 {
		opts.Limit = QueryLimitDefault
	}
	if opts.Limit > QueryLimitMax {
		opts.Limit = QueryLimitMax
	}
	return s.repo.QueryDocs(user, opts)
}

// IndexFile upsert 索引文档（Indexer 的落库入口；content 为空即仅名称）。
func (s *Store) IndexFile(fileID, versionID, ownerID uuid.UUID, spaceID *uuid.UUID, name, content string) error {
	return s.repo.UpsertDoc(Doc{FileID: fileID, VersionID: versionID, OwnerID: ownerID, SpaceID: spaceID, Name: name, Content: content})
}

// RemoveFile 删除索引文档（幂等；正常路径由外键级联承担，此为显式入口）。
func (s *Store) RemoveFile(fileID uuid.UUID) error {
	return s.repo.RemoveDoc(fileID)
}
