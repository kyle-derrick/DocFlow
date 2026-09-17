// Package caddytls 提供管理端 HTTPS 运行时切换：渲染与 deploy/Caddyfile
// 同构的配置文本，经 Caddy admin API（POST /load，text/caddyfile）热下发，
// 无需重启 caddy 容器。
//
// 模式（Mode）：
//   - http      站点 {$APP_DOMAIN::80}（env 驱动，本地验证默认，明文）；
//   - auto      站点 = 管理员填写的域名，Caddy 自动 ACME 签发受信证书
//     （要求公网域名 DNS 指向本机，80 端口做挑战/重定向）；
//   - internal  站点 = 域名或 IP + tls internal（Caddy 内部 CA 自签，
//     流量加密但浏览器不受信告警，适合内网/IP 部署）。
//
// 模板中 {$VAR} 占位符（APP_DOMAIN/CONTENT_DOMAIN/ONLYOFFICE_UPSTREAM 等）
// 保持原样下发，由 caddy 容器自身的 env 展开——backend 无需复制这些配置。
// 安全边界：admin API 地址仅限 compose 内网（caddy 只 expose 不映射宿主端口）。
package caddytls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Mode 为 TLS 运行模式。
type Mode string

const (
	ModeHTTP     Mode = "http"
	ModeAuto     Mode = "auto"
	ModeInternal Mode = "internal"
)

// State 为 tls_state 单行（id=1）：当前生效偏好的持久化。
type State struct {
	ID        uint       `gorm:"primaryKey"`
	Mode      Mode       `gorm:"column:mode"`
	Domain    string     `gorm:"column:domain"`
	UpdatedBy *uuid.UUID `gorm:"column:updated_by;type:uuid"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

func (State) TableName() string { return "tls_state" }

// ErrNotManaged 表示未配置 CADDY_ADMIN_ADDR（功能关闭，TLS 由部署配置决定）。
var ErrNotManaged = errors.New("caddy tls not managed: CADDY_ADMIN_ADDR empty")

// repository 持久化 tls_state（gorm / 内存双实现，模式参照 settings 包）。
type repository interface {
	GetState() (State, error)
	UpsertState(s State) error
}

type gormRepo struct{ db *gorm.DB }

func (g *gormRepo) GetState() (State, error) {
	var s State
	err := g.db.First(&s, "id = ?", 1).Error
	return s, err
}

func (g *gormRepo) UpsertState(s State) error {
	s.ID = 1
	return g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"mode", "domain", "updated_by", "updated_at"}),
	}).Create(&s).Error
}

// Service 组合渲染、下发与持久化。
type Service struct {
	repo      repository
	adminAddr string // 如 caddy:2019；空 = 未托管
	audit     audit.Recorder
	client    *http.Client
	now       func() time.Time
	retry     time.Duration // ReapplyStartup 重试间隔（默认 5s，测试注入缩短）
}

// NewService 创建服务；adminAddr 为空时 Get/Apply 返回 ErrNotManaged。
func NewService(db *gorm.DB, adminAddr string) *Service {
	return &Service{
		repo:      &gormRepo{db: db},
		adminAddr: strings.TrimPrefix(strings.TrimPrefix(adminAddr, "http://"), "https://"),
		client:    &http.Client{Timeout: 10 * time.Second},
		now:       time.Now,
		retry:     5 * time.Second,
	}
}

// SetAuditRecorder 注入审计记录器（可选）。
func (s *Service) SetAuditRecorder(r audit.Recorder) { s.audit = r }

// Get 返回当前持久化状态；无行时返回零值 http 模式（迁移已播种，兜底）。
func (s *Service) Get(ctx context.Context) (State, error) {
	st, err := s.repo.GetState()
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return State{ID: 1, Mode: ModeHTTP}, nil
	}
	return st, err
}

// 校验 domain：host 或 host:port；拒绝 scheme/路径/空白；auto 模式要求
// 域名（公共 ACME 不给纯 IP 签发），internal 允许 IP。
var domainPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(:[0-9]{1,5})?$`)
var ipv4Pattern = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)

func validateDomain(mode Mode, domain string) error {
	domain = strings.TrimSpace(domain)
	if mode == ModeHTTP {
		return nil
	}
	if !domainPattern.MatchString(domain) {
		return fmt.Errorf("domain %q invalid: host[:port], no scheme/path", domain)
	}
	host := strings.Split(domain, ":")[0]
	if mode == ModeAuto && ipv4Pattern.MatchString(host) {
		return fmt.Errorf("auto mode requires a public domain name (not an IP)")
	}
	return nil
}

// Apply 校验并热下发新配置：先 POST /load（失败即拒绝，库不落盘，caddy
// 保持旧配置——Caddy 对非法配置原子拒绝），成功后持久化并记审计。
func (s *Service) Apply(ctx context.Context, mode Mode, domain, actor string) (State, error) {
	if s.adminAddr == "" {
		return State{}, ErrNotManaged
	}
	mode = Mode(strings.TrimSpace(string(mode)))
	domain = strings.TrimSpace(domain)
	switch mode {
	case ModeHTTP, ModeAuto, ModeInternal:
	default:
		return State{}, fmt.Errorf("mode %q invalid: http|auto|internal", mode)
	}
	if err := validateDomain(mode, domain); err != nil {
		return State{}, err
	}
	if err := s.push(ctx, Render(mode, domain)); err != nil {
		return State{}, fmt.Errorf("caddy rejected config: %w", err)
	}
	var by *uuid.UUID
	if actor != "" {
		if id, err := uuid.Parse(actor); err == nil {
			by = &id
		}
	}
	st := State{ID: 1, Mode: mode, Domain: domain, UpdatedBy: by, UpdatedAt: s.now().UTC()}
	if err := s.repo.UpsertState(st); err != nil {
		return State{}, err
	}
	if s.audit != nil {
		metadata, _ := json.Marshal(map[string]string{"mode": string(mode), "domain": domain})
		_ = s.audit.Record(audit.Entry{UserID: by, Action: "tls.update", ResourceType: "caddy", ResourceID: "tls", Status: audit.StatusSuccess, Metadata: string(metadata)})
	}
	return st, nil
}

// push 向 Caddy admin API 下发 Caddyfile 文本（POST /load）。
func (s *Service) push(ctx context.Context, caddyfile string) error {
	url := "http://" + s.adminAddr + "/load"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(caddyfile))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// ReapplyStartup 启动期对账：持久化偏好在 http 以外时，向 caddy 重新下发
// （覆盖 caddy 容器先于 backend 单独重启回到 env 初始态的窗口；caddy 就绪
// 晚于 backend 时重试）。放弃即退出（管理员可在页面重新保存）。
func (s *Service) ReapplyStartup(ctx context.Context) {
	if s.adminAddr == "" {
		return
	}
	st, err := s.Get(ctx)
	if err != nil || st.Mode == ModeHTTP {
		return
	}
	for i := 0; i < 12; i++ {
		if err := s.push(ctx, Render(st.Mode, st.Domain)); err == nil {
			log.Printf("caddy tls re-applied: mode=%s domain=%s", st.Mode, st.Domain)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.retry):
		}
	}
	log.Printf("caddy tls re-apply gave up: mode=%s domain=%s (re-save in admin UI)", st.Mode, st.Domain)
}
