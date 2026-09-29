// studio.go：Studio 项目注册表的 HTTP 端点（业务在 internal/studio）。
//
//	GET    /api/v1/studio/projects      列出本人项目（updated_at 倒序）
//	POST   /api/v1/studio/projects      新建（服务端生成 id）
//	PUT    /api/v1/studio/projects/:id  覆盖式更新（属主校验）
//	DELETE /api/v1/studio/projects/:id  删除（属主校验；不影响文件本体）
//
// handler 只做参数解析/属主绑定/响应码；校验错误 400（INVALID_STUDIO_PROJECT），
// 非属主/不存在一律 404（不区分两者，避免探测他人项目 id）。
package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/studio"
)

// studioStore 抽象项目存储（生产实现 *studio.Store；测试可注入）。
type studioStore interface {
	List(user uuid.UUID) ([]studio.Project, error)
	Create(user uuid.UUID, p studio.Project) (studio.Project, error)
	Update(user, id uuid.UUID, p studio.Project) (studio.Project, bool, error)
	Delete(user, id uuid.UUID) (bool, error)
}

// studioProjectPayload 为 POST/PUT 载荷（字段见 studio.Project；id/user 由
// 服务端绑定，载荷中的 id 恒忽略）。
type studioProjectPayload struct {
	Name         string `json:"name"`
	SpaceID      string `json:"space_id"`
	RootFolderID string `json:"root_folder_id"`
	SpaceName    string `json:"space_name"`
	FolderPath   string `json:"folder_path"`
	Engine       string `json:"engine"`
	Harness      string `json:"harness"`
	Model        string `json:"model"`
}

// toProject 载荷 → 领域对象（space/folder 须为合法 uuid）。
func (p studioProjectPayload) toProject() (studio.Project, error) {
	spaceID, err := uuid.Parse(p.SpaceID)
	if err != nil {
		return studio.Project{}, errors.New("space_id 须为合法 UUID")
	}
	rootID, err := uuid.Parse(p.RootFolderID)
	if err != nil {
		return studio.Project{}, errors.New("root_folder_id 须为合法 UUID")
	}
	return studio.Project{
		Name: p.Name, SpaceID: spaceID, RootFolder: rootID,
		SpaceName: p.SpaceName, FolderPath: p.FolderPath,
		Engine: p.Engine, Harness: p.Harness, Model: p.Model,
	}, nil
}

// studioProjectsList GET /studio/projects。
func (h *Handler) studioProjectsList(c *gin.Context) {
	if h.studioProjects == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "studio store is not configured"})
		return
	}
	rows, err := h.studioProjects.List(userID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list studio projects"})
		return
	}
	if rows == nil {
		rows = []studio.Project{}
	}
	c.JSON(http.StatusOK, gin.H{"projects": rows})
}

// studioProjectsCreate POST /studio/projects。
func (h *Handler) studioProjectsCreate(c *gin.Context) {
	if h.studioProjects == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "studio store is not configured"})
		return
	}
	var payload studioProjectPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	proj, err := payload.toProject()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_STUDIO_PROJECT"})
		return
	}
	created, err := h.studioProjects.Create(userID(c), proj)
	if err != nil {
		if errors.Is(err, studio.ErrInvalidProject) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_STUDIO_PROJECT"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to create studio project"})
		return
	}
	c.JSON(http.StatusCreated, created)
}

// studioProjectsUpdate PUT /studio/projects/:id。
func (h *Handler) studioProjectsUpdate(c *gin.Context) {
	if h.studioProjects == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "studio store is not configured"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var payload studioProjectPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	proj, err := payload.toProject()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_STUDIO_PROJECT"})
		return
	}
	updated, found, err := h.studioProjects.Update(userID(c), id, proj)
	if err != nil {
		if errors.Is(err, studio.ErrInvalidProject) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_STUDIO_PROJECT"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to update studio project"})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "studio project not found"})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// studioProjectsDelete DELETE /studio/projects/:id。
func (h *Handler) studioProjectsDelete(c *gin.Context) {
	if h.studioProjects == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "studio store is not configured"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	found, err := h.studioProjects.Delete(userID(c), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to delete studio project"})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "studio project not found"})
		return
	}
	c.Status(http.StatusNoContent)
}
