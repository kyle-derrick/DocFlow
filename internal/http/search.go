package http

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/docflow/docflow/internal/search"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// searchService 抽象全文检索能力；生产实现为 *search.Store（SetSearch 注入）。
type searchService interface {
	Query(user uuid.UUID, opts search.QueryOptions) ([]search.Result, error)
}

// searchFiles GET /api/v1/search?q=&limit=：文件名 + 文本内容全文检索。
// 覆盖个人 + 团队可读文件（访问判定复用 SearchAccessible 模式，实时生效），
// 软删文件排除。二进制（图片/pdf/office）仅名称匹配（索引侧无内容）。
// tag_id/starred 过滤可选（语义同 /files 检索模式）；limit 缺省 20、上限 100。
// 高频读端点：不记录审计；限流沿用 api 组。
func (h *Handler) searchFiles(c *gin.Context) {
	if h.search == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "search unavailable"})
		return
	}
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing query"})
		return
	}
	limit := search.QueryLimitDefault
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if n < limit {
			limit = n
		}
	}
	tagID, ok := h.parseTagFilter(c)
	if !ok {
		return
	}
	starred, ok := parseStarredFilter(c)
	if !ok {
		return
	}
	results, err := h.search.Query(userID(c), search.QueryOptions{Q: q, TagID: tagID, Starred: starred, Limit: limit})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to search"})
		return
	}
	out := make([]gin.H, 0, len(results))
	for _, r := range results {
		item := gin.H{"id": r.ID, "name": r.Name, "type": r.Type, "parent_id": r.ParentID, "updated_at": r.UpdatedAt}
		if r.Snippet != "" {
			item["snippet"] = r.Snippet
		}
		out = append(out, item)
	}
	c.JSON(http.StatusOK, gin.H{"results": out})
}
