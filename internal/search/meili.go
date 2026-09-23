package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MeiliIndexUID 为 DocFlow 检索索引的固定标识（主键 file_id）。
const MeiliIndexUID = "docflow-files"

// meiliTimeout 单次 Meilisearch 请求超时。
const meiliTimeout = 10 * time.Second

// ErrUnsupportedFilter Meilisearch 驱动不支持 tag/starred 过滤（索引不含
// 相应字段；返回错误而非静默忽略，避免放宽结果集）。
var ErrUnsupportedFilter = fmt.Errorf("meili driver does not support tag/starred filters")

var _ Repo = (*MeiliRepo)(nil)

// MeiliRepo 是 Repo 的 Meilisearch 实现（SEARCH_DRIVER=meili，v2 可选项）：
//   - 文档字段 {file_id, version_id, owner_id, space_id, name, content}，
//     space_id 为空时 JSON null——Meili 忽略，space 过滤不命中；
//   - 访问过滤 owner_id = user OR space_id IN（用户空间列表，spacesOf 注入，
//     由 main 接 space.Store.ListForUser 提供——不扩 Repo.QueryDocs 签名，
//     pg/memory 实现与既有调用方零改动）；
//   - tag/starred 过滤不支持（ErrUnsupportedFilter）；
//   - Result：name 取索引值（rename 后需等待索引重建，与 pg 的实时 JOIN
//     语义有差异）；type 恒 file（目录不入索引）；parent_id/updated_at
//     不在索引中，返回零值；snippet 取 _formatted.content（[[..]] 高亮
//     标记与 pg 对齐，名称命中时回退名称）。
type MeiliRepo struct {
	base   string
	apiKey string
	client *http.Client
	// spacesOf 返回用户所在空间列表（访问过滤数据源，main 注入）。
	spacesOf func(user uuid.UUID) ([]uuid.UUID, error)
}

// NewMeiliRepo 构造 Meilisearch Repo；baseURL 形如 http://meilisearch:7700
// （不含尾斜杠），apiKey 可空（未设 MASTER_KEY 的实例）；spacesOf 不可为 nil。
func NewMeiliRepo(baseURL, apiKey string, spacesOf func(user uuid.UUID) ([]uuid.UUID, error)) *MeiliRepo {
	return &MeiliRepo{
		base:     strings.TrimSuffix(baseURL, "/"),
		apiKey:   apiKey,
		client:   &http.Client{Timeout: meiliTimeout},
		spacesOf: spacesOf,
	}
}

// meiliError 解析 Meilisearch 错误响应（{"message":...}）。
type meiliError struct {
	Message string `json:"message"`
}

// do 发送 JSON 请求并读响应体；非 2xx 返回携带状态码的错误。
func (m *MeiliRepo) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var me meiliError
		_ = json.Unmarshal(data, &me)
		return nil, fmt.Errorf("meilisearch %s %s: status %d: %s", method, path, resp.StatusCode, me.Message)
	}
	return data, nil
}

// EnsureIndex 创建检索索引（主键 file_id）并启用访问过滤字段；已存在
// （Meilisearch 405 index_already_exists）幂等成功。启动时调用（main）。
// filterableAttributes 必须显式设置：Meilisearch 默认索引无任何可过滤
// 属性，search 带 filter 一律 invalid_search_filter（真实实例暴露；
// 单测的假 HTTP 服务无法覆盖该服务端状态语义）。
func (m *MeiliRepo) EnsureIndex(ctx context.Context) error {
	_, err := m.do(ctx, http.MethodPost, "/indexes", map[string]string{"uid": MeiliIndexUID, "primaryKey": "file_id"})
	if err != nil {
		// 索引已存在：Meilisearch 返回 405（index_already_exists），幂等忽略。
		if !strings.Contains(err.Error(), "status 405") {
			return err
		}
	}
	// 幂等重设过滤字段（重复设同值无副作用）；异步生效，首个文档入索引前
	// 由启动顺序保证（EnsureIndex 先于任何 UpsertDoc）。注意 v1.8 的
	// settings 子路由仅支持 PUT（PATCH 为更高版本行为，返回 405）。
	_, err = m.do(ctx, http.MethodPut, "/indexes/"+MeiliIndexUID+"/settings/filterable-attributes",
		[]string{"owner_id", "space_id"})
	return err
}

// meiliDoc 为索引文档（字段见 MeiliRepo 注释；ID 类字段序列化为字符串）。
type meiliDoc struct {
	FileID    string  `json:"file_id"`
	VersionID string  `json:"version_id"`
	OwnerID   string  `json:"owner_id"`
	SpaceID   *string `json:"space_id"`
	Name      string  `json:"name"`
	Content   string  `json:"content"`
}

// UpsertDoc upsert 索引文档（按主键 file_id 全量覆盖）。
func (m *MeiliRepo) UpsertDoc(d Doc) error {
	doc := meiliDoc{
		FileID: d.FileID.String(), VersionID: d.VersionID.String(),
		OwnerID: d.OwnerID.String(), Name: d.Name, Content: d.Content,
	}
	if d.SpaceID != nil {
		id := d.SpaceID.String()
		doc.SpaceID = &id
	}
	_, err := m.do(context.Background(), http.MethodPost, "/indexes/"+MeiliIndexUID+"/documents?primaryKey=file_id", []meiliDoc{doc})
	return err
}

// RemoveDoc 删除 fileID 的索引文档（幂等）。
func (m *MeiliRepo) RemoveDoc(fileID uuid.UUID) error {
	_, err := m.do(context.Background(), http.MethodDelete, "/indexes/"+MeiliIndexUID+"/documents/"+fileID.String(), nil)
	return err
}

// meiliSearchRequest 查询请求体。
type meiliSearchRequest struct {
	Q                     string   `json:"q"`
	Limit                 int      `json:"limit"`
	Filter                string   `json:"filter,omitempty"`
	AttributesToHighlight []string `json:"attributesToHighlight,omitempty"`
	HighlightPreTag       string   `json:"highlightPreTag,omitempty"`
	HighlightPostTag      string   `json:"highlightPostTag,omitempty"`
}

// meiliHit 单条命中（_formatted 为高亮渲染结果）。
type meiliHit struct {
	FileID    string `json:"file_id"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	Formatted struct {
		Content string `json:"content"`
	} `json:"_formatted"`
}

type meiliSearchResponse struct {
	Hits []meiliHit `json:"hits"`
}

// QueryDocs 检索 user 可读且命中 q 的文件：访问过滤 owner_id = user OR
// space_id IN 用户空间列表；limit 截断；tag/starred 过滤不支持
// （ErrUnsupportedFilter）。
func (m *MeiliRepo) QueryDocs(user uuid.UUID, opts QueryOptions) ([]Result, error) {
	if opts.TagID != nil || opts.Starred != nil {
		return nil, ErrUnsupportedFilter
	}
	spaces, err := m.spacesOf(user)
	if err != nil {
		return nil, err
	}
	filter := "owner_id = " + quote(user.String())
	if opts.SpaceID != nil {
		filter = "space_id = " + quote(opts.SpaceID.String())
		// 空间过滤蕴含成员语义：owner 命中但不在该空间的个人文件须排除，
		// 故直接以 space_id 为准（个人空间本身也是一个 space）。
	} else if len(spaces) > 0 {
		ids := make([]string, 0, len(spaces))
		for _, id := range spaces {
			ids = append(ids, quote(id.String()))
		}
		filter += " OR space_id IN [" + strings.Join(ids, ", ") + "]"
	}
	body, err := m.do(context.Background(), http.MethodPost, "/indexes/"+MeiliIndexUID+"/search", meiliSearchRequest{
		Q: opts.Q, Limit: opts.Limit, Filter: filter,
		AttributesToHighlight: []string{"content"},
		HighlightPreTag:       "[[", HighlightPostTag: "]]",
	})
	if err != nil {
		return nil, err
	}
	var out meiliSearchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(out.Hits))
	for _, hit := range out.Hits {
		id, err := uuid.Parse(hit.FileID)
		if err != nil {
			continue // 防御：跳过无法解析的主键
		}
		results = append(results, Result{
			ID:   id,
			Name: hit.Name,
			Type: "file",
			// parent_id/updated_at 未入索引（v1 子集），返回零值。
			Snippet: meiliSnippet(hit, opts.Q),
		})
	}
	return results, nil
}

// meiliSnippet 生成命中上下文：内容高亮（[[..]] 标记）优先；名称命中回退
// 名称（与 pg 的名称 snippet 语义对齐）；均无命中片段时为空。
func meiliSnippet(hit meiliHit, q string) string {
	if strings.Contains(hit.Formatted.Content, "[[") {
		return hit.Formatted.Content
	}
	if containsFold(hit.Name, q) {
		return hit.Name
	}
	return ""
}

// quote 生成 Meilisearch filter 的字符串字面量（UUID 仅含安全字符）。
func quote(v string) string { return `"` + v + `"` }
