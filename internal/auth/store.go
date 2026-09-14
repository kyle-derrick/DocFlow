package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type GormSessionStore struct {
	db *gorm.DB
}

// ErrUserNotFound 表示按 ID 查询用户不存在（Role 查询使用）。
var ErrUserNotFound = errors.New("user not found")

func NewGormSessionStore(db *gorm.DB) *GormSessionStore {
	return &GormSessionStore{db: db}
}

func (s *GormSessionStore) Create(session Session, info SessionInfo) error {
	if err := s.db.Create(&session).Error; err != nil {
		return err
	}
	// ip/user_agent 为审计性元数据（best-effort）：写入失败不影响会话可用性。
	// ip 为 INET 列，*string 参数写入与 audit_logs.ip 同一模式。
	if info.IP == "" && info.UserAgent == "" {
		return nil
	}
	if info.IP != "" {
		ip := info.IP
		session.IP = &ip
	}
	session.UserAgent = info.UserAgent
	return s.db.Model(&Session{}).Where("id = ?", session.ID).
		Updates(map[string]any{"ip": session.IP, "user_agent": session.UserAgent}).Error
}

// ListActive 返回 user 的全部活跃会话（未撤销且未过期，last_active_at 倒序）。
// ip 经 host() 归一为文本；不读取 refresh_token_hash（凭据材料不出服务端）。
func (s *GormSessionStore) ListActive(userID uuid.UUID, now time.Time) ([]SessionView, error) {
	var out []SessionView
	err := s.db.Raw(
		"SELECT id, created_at, last_active_at, expires_at, host(ip) AS ip, user_agent FROM sessions WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ? ORDER BY last_active_at DESC, id",
		userID, now,
	).Scan(&out).Error
	return out, err
}

// RevokeByID 撤销属主 own 的指定会话（仅未撤销时生效）；会话不存在、
// 非属主或已撤销返回 false（HTTP 层统一映射 404，不泄露存在性）。
func (s *GormSessionStore) RevokeByID(owner, id uuid.UUID, now time.Time) (bool, error) {
	result := s.db.Model(&Session{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", id, owner).
		Update("revoked_at", now)
	return result.RowsAffected == 1, result.Error
}

// Rotate 原子轮换 refresh token 哈希（单条 session 记录即一个 token family）：
//   - 事务内 SELECT ... FOR UPDATE（clause.Locking）行锁串行化并发轮换；
//   - UPDATE WHERE 复查 refresh_token_hash：RowsAffected != 1 说明该行已被
//     并发事务轮换（本次持旧 hash 即为重放），此时撤销整个 session
//     （revoked_at=now，token family 全部失效）并返回 ErrInvalidRefreshToken；
//     事务须以 nil 错误返回以提交撤销，错误在事务提交后返回；
//   - 旧 hash 未命中（已被轮换/撤销/过期或本就未知）同样返回
//     ErrInvalidRefreshToken——此时无法由旧 hash 反查 session 定位 family，
//     撤销只能覆盖上述 UPDATE 复查命中的重放窗口。
func (s *GormSessionStore) Rotate(tokenHash, replacementHash string, now, expiresAt time.Time) (Session, error) {
	var session Session
	replayed := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 显式列清单：sessions.ip 为 INET 列，扫描进 *string 依赖驱动解码，
		// 轮换路径不需要它（及 user_agent），限定列以规避并保持行为不变。
		result := tx.Select("id", "user_id", "created_at", "last_active_at", "expires_at", "revoked_at").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("refresh_token_hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash, now).
			First(&session)
		if result.Error != nil {
			return result.Error
		}
		result = tx.Model(&Session{}).
			Where("id = ? AND refresh_token_hash = ? AND revoked_at IS NULL", session.ID, tokenHash).
			Updates(map[string]any{"refresh_token_hash": replacementHash, "last_active_at": now, "expires_at": expiresAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			if err := tx.Model(&Session{}).Where("id = ?", session.ID).Update("revoked_at", now).Error; err != nil {
				return err
			}
			replayed = true
			return nil // 提交事务以持久化撤销，错误延后到事务外返回
		}
		return nil
	})
	if replayed {
		return Session{}, ErrInvalidRefreshToken
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Session{}, ErrInvalidRefreshToken
	}
	return session, err
}

func (s *GormSessionStore) Revoke(tokenHash string, now time.Time) error {
	result := s.db.Model(&Session{}).Where("refresh_token_hash = ? AND revoked_at IS NULL", tokenHash).
		Update("revoked_at", now)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrInvalidRefreshToken
	}
	return nil
}

// RevokeAllForUser 撤销 user 的全部未撤销会话；exceptHash 非空时保留该
// refresh token hash 对应的会话（改密场景保留当前会话）。
func (s *GormSessionStore) RevokeAllForUser(userID uuid.UUID, exceptHash string, now time.Time) error {
	query := s.db.Model(&Session{}).Where("user_id = ? AND revoked_at IS NULL", userID)
	if exceptHash != "" {
		query = query.Where("refresh_token_hash <> ?", exceptHash)
	}
	return query.Update("revoked_at", now).Error
}

// GormPasswordResetStore 是 PasswordResetStore 的 PostgreSQL 实现
// （password_reset_tokens 表见 migrations/014）。
type GormPasswordResetStore struct{ db *gorm.DB }

func NewGormPasswordResetStore(db *gorm.DB) *GormPasswordResetStore {
	return &GormPasswordResetStore{db: db}
}

func (s *GormPasswordResetStore) Create(token PasswordResetToken) error {
	return s.db.Create(&token).Error
}

// Consume 原子标记 used_at：先按 token_hash 读取属主，再以
// used_at IS NULL AND expires_at > now 条件更新；RowsAffected != 1 说明
// 令牌已被并发消费/使用/过期，返回 false（一次性语义）。
func (s *GormPasswordResetStore) Consume(tokenHash string, now time.Time) (uuid.UUID, bool, error) {
	var token PasswordResetToken
	if err := s.db.First(&token, "token_hash = ?", tokenHash).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	result := s.db.Model(&PasswordResetToken{}).
		Where("id = ? AND used_at IS NULL AND expires_at > ?", token.ID, now).
		Update("used_at", now)
	if result.Error != nil {
		return uuid.Nil, false, result.Error
	}
	return token.UserID, result.RowsAffected == 1, nil
}

// GormAPITokenStore 是 TokenStore 的 PostgreSQL 实现
// （api_tokens 表见 migrations/016）。
type GormAPITokenStore struct{ db *gorm.DB }

func NewGormAPITokenStore(db *gorm.DB) *GormAPITokenStore {
	return &GormAPITokenStore{db: db}
}

func (s *GormAPITokenStore) Create(token APIToken) error {
	return s.db.Create(&token).Error
}

// List 返回 owner 的未撤销令牌（含已过期，由调用方按 expires_at 呈现），
// 按创建时间倒序。
func (s *GormAPITokenStore) List(owner uuid.UUID) ([]APIToken, error) {
	var out []APIToken
	err := s.db.Where("user_id = ? AND revoked_at IS NULL", owner).
		Order("created_at DESC, id").Find(&out).Error
	return out, err
}

// Revoke 撤销属主的令牌（仅未撤销时生效）；不存在、非属主或已撤销返回 false。
func (s *GormAPITokenStore) Revoke(owner, id uuid.UUID, now time.Time) (bool, error) {
	result := s.db.Model(&APIToken{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", id, owner).
		Update("revoked_at", now)
	return result.RowsAffected == 1, result.Error
}

func (s *GormAPITokenStore) Update(owner, id uuid.UUID, name *string, scopes *[]string) (APIToken, error) {
	fields := map[string]any{}
	if name != nil {
		fields["name"] = *name
	}
	if scopes != nil {
		fields["scopes"] = *scopes
	}
	result := s.db.Model(&APIToken{}).Where("id = ? AND user_id = ? AND revoked_at IS NULL", id, owner).Updates(fields)
	if result.Error != nil {
		return APIToken{}, result.Error
	}
	if result.RowsAffected == 0 {
		return APIToken{}, gorm.ErrRecordNotFound
	}
	var token APIToken
	if err := s.db.Where("id = ?", id).First(&token).Error; err != nil {
		return APIToken{}, err
	}
	return token, nil
}

// FindActiveByPrefix 按 prefix 定位唯一候选行（未撤销且未过期），仅取认证
// 所需最小字段（id/user_id/token_hash）。prefix 碰撞概率极低（8 字符
// base64url ≈ 2^48），命中多行时取首行，随后仍以全量哈希常量时间比对裁决。
func (s *GormAPITokenStore) FindActiveByPrefix(prefix string, now time.Time) (PATLookup, bool, error) {
	var token APIToken
	err := s.db.Select("id", "user_id", "token_hash").
		Where("prefix = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", prefix, now).
		Order("id").First(&token).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PATLookup{}, false, nil
	}
	if err != nil {
		return PATLookup{}, false, err
	}
	return PATLookup{ID: token.ID, UserID: token.UserID, TokenHash: token.TokenHash, Scopes: token.Scopes}, true, nil
}

// TouchLastUsed 更新 last_used_at 单列（UpdateColumn：跳过 hooks、不联动
// updated_at）。best-effort：错误不上抛，不阻塞认证路径。
func (s *GormAPITokenStore) TouchLastUsed(id uuid.UUID) {
	_ = s.db.Model(&APIToken{}).Where("id = ?", id).
		UpdateColumn("last_used_at", time.Now().UTC()).Error
}

// DeleteExpired 删除 expires_at 或 revoked_at 早于阈值（now-30d）的令牌行，
// 返回删除行数。expires_at 为 NULL（永久）且未撤销的行不会被删除。
func (s *GormAPITokenStore) DeleteExpired(now time.Time) (int64, error) {
	cutoff := now.Add(-PATRetention)
	result := s.db.Exec("DELETE FROM api_tokens WHERE (expires_at IS NOT NULL AND expires_at < ?) OR (revoked_at IS NOT NULL AND revoked_at < ?)", cutoff, cutoff)
	return result.RowsAffected, result.Error
}

type UserStore struct {
	db *gorm.DB
	// quotaProvider 为新用户开户默认配额的热读取（main 注入，读取
	// system_settings 的 upload.default_quota）；nil 时回退 DefaultStorageQuota。
	quotaProvider func() int64
}

func NewUserStore(db *gorm.DB) *UserStore {
	return &UserStore{db: db}
}

// SetDefaultQuotaProvider 注入新用户开户默认配额热读取（幂等；nil 不覆盖）。
// CreateUser 对未显式设置配额（<=0）的新用户应用该值（仍非法则回退常量）。
func (s *UserStore) SetDefaultQuotaProvider(fn func() int64) {
	if fn != nil {
		s.quotaProvider = fn
	}
}

// applyDefaultQuota 为新用户回填默认配额（<=0 时生效）。
func (s *UserStore) applyDefaultQuota(quota int64) int64 {
	if quota > 0 {
		return quota
	}
	if s.quotaProvider != nil {
		if n := s.quotaProvider(); n > 0 {
			return n
		}
	}
	return DefaultStorageQuota
}

// FindActiveByEmail 按邮箱精确查找 active 用户；邮箱统一小写归一后匹配
// （注册/seed 侧写入即为归一值，登录输入大小写不敏感）。
func (s *UserStore) FindActiveByEmail(email string) (User, error) {
	var user User
	err := s.db.Where("email = ? AND status = ?", NormalizeEmail(email), "active").First(&user).Error
	return user, err
}

// FindActiveByIdentifier 按登录标识查找 active 用户（C21a username 登录）：
// 含 @ 按归一邮箱精确匹配，否则按用户名精确匹配。自动锁定
// （locked_until 未到期）的账号仍会返回，由调用方检查锁定期并回 423；
// 用户名不存在或非 active 与邮箱路径同返回 gorm.ErrRecordNotFound。
func (s *UserStore) FindActiveByIdentifier(identifier string) (User, error) {
	ident := strings.TrimSpace(identifier)
	var user User
	var err error
	if strings.Contains(ident, "@") {
		err = s.db.Where("email = ? AND status = ?", NormalizeEmail(ident), StatusActive).First(&user).Error
	} else {
		err = s.db.Where("username = ? AND status = ?", ident, StatusActive).First(&user).Error
	}
	return user, err
}

// IsLocked 判断锁定截止时间是否仍生效（C9）：locked_until 非空且晚于 now。
func IsLocked(lockedUntil *time.Time, now time.Time) bool {
	return lockedUntil != nil && lockedUntil.After(now)
}

// GetByID 按 ID 返回完整用户记录（含密码哈希）；不存在返回 ErrUserNotFound。
// 供改密（校验旧密码）等需要凭据的场景使用。
func (s *UserStore) GetByID(id uuid.UUID) (User, error) {
	var user User
	err := s.db.First(&user, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return User{}, ErrUserNotFound
	}
	return user, err
}

// UpdatePasswordHash 更新用户密码哈希（改密/重置链路）。
func (s *UserStore) UpdatePasswordHash(id uuid.UUID, passwordHash string) error {
	return s.db.Model(&User{}).Where("id = ?", id).Updates(map[string]any{"password_hash": passwordHash, "updated_at": time.Now().UTC()}).Error
}

// pgUniqueViolation 为 PostgreSQL 唯一约束冲突错误码（23505）。
const pgUniqueViolation = "23505"

// CreateUser 写入新用户（邀请接受注册 / OIDC 自动开户）。用户名或邮箱已被
// 占用（任意状态的用户均占用唯一约束）返回 ErrUserExists；未显式设置配额的
// 新用户回填开户默认配额（upload.default_quota 热读取，缺省 10GiB），
// language/timezone 空值回退默认（zh-CN / Asia/Shanghai）。
func (s *UserStore) CreateUser(u User) error {
	u.StorageQuota = s.applyDefaultQuota(u.StorageQuota)
	if u.Language == "" {
		u.Language = "zh-CN"
	}
	if u.Timezone == "" {
		u.Timezone = "Asia/Shanghai"
	}
	if err := s.db.Create(&u).Error; err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return ErrUserExists
		}
		return err
	}
	return nil
}

// likePrefixPattern 构造前缀匹配的 LIKE 模式：转义 \、%、_ 通配符后追加 %。
func likePrefixPattern(q string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(q) + "%"
}

// lookupCondition 构造 Lookup 的匹配条件与参数：
//   - q 含 @：仅邮箱整串精确匹配（lower(email) = lower(q)），防止以邮箱
//     片段做前缀探测枚举用户；
//   - 否则：邮箱整串精确匹配 或 用户名前缀 ILIKE（ILIKE 自身大小写不敏感）。
func lookupCondition(q string) (string, []any) {
	email := strings.ToLower(q)
	if strings.Contains(q, "@") {
		return "lower(email) = ?", []any{email}
	}
	return "(lower(email) = ? OR username ILIKE ? ESCAPE '\\')", []any{email, likePrefixPattern(q)}
}

// Lookup 按邮箱整串精确匹配（大小写不敏感）或用户名前缀匹配活跃用户，
// 按 username 排序、最多 limit 条；供用户查找/邀请场景使用。
// 防邮箱枚举：email 永不做前缀/模糊匹配；q 含 @ 时仅按邮箱精确匹配。
// 注意：返回的 User 含 email，调用方（HTTP 层）序列化时只暴露 id 与 username。
func (s *UserStore) Lookup(q string, limit int) ([]User, error) {
	if limit <= 0 {
		limit = 10
	}
	condition, args := lookupCondition(q)
	var out []User
	err := s.db.
		Where("status = ?", StatusActive).
		Where(condition, args...).
		Order("username, id").Limit(limit).Find(&out).Error
	return out, err
}

// UsernameExists 判断用户名是否已被占用（任意状态的用户均占用唯一约束）；
// 供 OIDC 自动开户的用户名去重使用。
func (s *UserStore) UsernameExists(username string) (bool, error) {
	var count int64
	err := s.db.Model(&User{}).Where("username = ?", username).Count(&count).Error
	return count > 0, err
}

// Status 返回用户状态（active/disabled/locked）；用户不存在返回 ErrUserNotFound。
// locked_until 未到期的账号派生为 locked（自动锁定与管理员置 status=locked
// 同语义，设计 6.1.3/7.3）：refresh 轮换成功后据此复查——禁用 401、锁定 423。
func (s *UserStore) Status(id uuid.UUID) (string, error) {
	var user User
	err := s.db.Select("status", "locked_until").First(&user, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", err
	}
	if user.Status == StatusActive && IsLocked(user.LockedUntil, time.Now().UTC()) {
		return StatusLocked, nil
	}
	return user.Status, nil
}

// Username 返回用户名（不含 email 等其他字段）；用户不存在时返回错误。
func (s *UserStore) Username(id uuid.UUID) (string, error) {
	var user User
	err := s.db.Select("username").First(&user, "id = ?", id).Error
	if err != nil {
		return "", err
	}
	return user.Username, nil
}

// Role 返回用户角色（user/admin）；用户不存在时返回 ErrUserNotFound，
// 供 RequireRole 中间件鉴权使用。
func (s *UserStore) Role(id uuid.UUID) (string, error) {
	var user User
	err := s.db.Select("role").First(&user, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", err
	}
	return user.Role, nil
}

// RecordLoginFailure 记录一次登录失败（C9）：failed_login_count 原子 +1；
// 达到 maxRetries 时置 locked_until = now + lockFor 并把计数清零（解锁后
// 重新累计）。锁定期间调用方不再计数（调用方先检查锁定态）。
func (s *UserStore) RecordLoginFailure(id uuid.UUID, maxRetries int, lockFor time.Duration) error {
	if maxRetries < 1 {
		maxRetries = 1
	}
	return s.db.Exec(`UPDATE users SET
		failed_login_count = CASE WHEN failed_login_count + 1 >= ? THEN 0 ELSE failed_login_count + 1 END,
		locked_until = CASE WHEN failed_login_count + 1 >= ? THEN ? ELSE locked_until END,
		updated_at = now()
		WHERE id = ?`, maxRetries, maxRetries, time.Now().UTC().Add(lockFor), id).Error
}

// ClearLoginFailures 清零登录失败计数并解除自动锁定（成功登录时调用）。
func (s *UserStore) ClearLoginFailures(id uuid.UUID) error {
	return s.db.Model(&User{}).Where("id = ?", id).
		Updates(map[string]any{"failed_login_count": 0, "locked_until": nil, "updated_at": time.Now().UTC()}).Error
}

// StorageQuota 返回用户存储配额（字节）；用户不存在返回 ErrUserNotFound。
func (s *UserStore) StorageQuota(id uuid.UUID) (int64, error) {
	var user User
	err := s.db.Select("storage_quota").First(&user, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, ErrUserNotFound
	}
	if err != nil {
		return 0, err
	}
	return user.StorageQuota, nil
}

// UpdateProfile 更新用户档案（C21a）：仅写入提供的字段（nil 跳过）；文本字段
// 去首尾空白后空串归一为 NULL（清空），language/timezone 空串跳过（列 NOT
// NULL）。用户不存在返回 ErrUserNotFound；校验由调用方先行
// （ProfileUpdate.Validate）。
func (s *UserStore) UpdateProfile(id uuid.UUID, update ProfileUpdate) error {
	fields := map[string]any{"updated_at": time.Now().UTC()}
	nullable := map[*string]string{
		update.Nickname:   "nickname",
		update.Department: "department",
		update.Position:   "position",
		update.Phone:      "phone",
		update.Bio:        "bio",
	}
	for value, column := range nullable {
		if value == nil {
			continue
		}
		trimmed := strings.TrimSpace(*value)
		if trimmed == "" {
			fields[column] = nil
			continue
		}
		fields[column] = trimmed
	}
	if update.Language != nil && strings.TrimSpace(*update.Language) != "" {
		fields["language"] = strings.TrimSpace(*update.Language)
	}
	if update.Timezone != nil && strings.TrimSpace(*update.Timezone) != "" {
		fields["timezone"] = strings.TrimSpace(*update.Timezone)
	}
	result := s.db.Model(&User{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// AdminUserUpdate 为管理端用户更新字段（C6）：指针区分「未提供」与零值；
// 取值合法性（status/role 枚举、quota 范围）由 HTTP 层先行校验。
type AdminUserUpdate struct {
	Status       *string
	StorageQuota *int64
	Role         *string
}

// AdminUpdateUser 管理端更新用户（C6）：status（active|disabled）、配额与
// 角色按需更新；置 active 同时清零锁定（视为管理员解锁）。用户不存在返回
// ErrUserNotFound。
func (s *UserStore) AdminUpdateUser(id uuid.UUID, update AdminUserUpdate) error {
	fields := map[string]any{"updated_at": time.Now().UTC()}
	if update.Status != nil {
		fields["status"] = *update.Status
		if *update.Status == StatusActive {
			fields["failed_login_count"] = 0
			fields["locked_until"] = nil
		}
	}
	if update.StorageQuota != nil {
		fields["storage_quota"] = *update.StorageQuota
	}
	if update.Role != nil {
		fields["role"] = *update.Role
	}
	result := s.db.Model(&User{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// AdminListUsers 管理端用户列表（C6）：q 非空时按 username/email 前缀 ILIKE
// 匹配（管理端场景允许邮箱前缀检索，likePrefixPattern 已转义通配符）；
// created_at 倒序分页，返回当前过滤条件下的总数。limit<=0 或超上限取 50。
func (s *UserStore) AdminListUsers(q string, limit, offset int) ([]User, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	filter := func(db *gorm.DB) *gorm.DB {
		query := strings.TrimSpace(q)
		if query == "" {
			return db
		}
		pattern := likePrefixPattern(query)
		return db.Where("username ILIKE ? ESCAPE '\\' OR email ILIKE ? ESCAPE '\\'", pattern, pattern)
	}
	var total int64
	if err := s.db.Model(&User{}).Scopes(filter).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var out []User
	err := s.db.Scopes(filter).Order("created_at DESC, id").Limit(limit).Offset(offset).Find(&out).Error
	return out, total, err
}
