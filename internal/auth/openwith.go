package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// OpenWithPreference 为用户「默认打开方式」偏好（user_open_with，migration 034）：
// 按扩展名记录前端打开器标识。
type OpenWithPreference struct {
	Ext       string    `json:"ext"`
	Opener    string    `json:"opener"`
	UpdatedAt time.Time `json:"updated_at"`
}

// openWithOpeners 为 opener 枚举白名单（前端打开器标识）。
var openWithOpeners = map[string]bool{
	"office": true, "drawio": true, "excalidraw": true, "text": true,
	"markdown": true, "code": true, "web": true, "default": true,
}

var (
	// ErrInvalidOpenWithExt 表示扩展名非法（去点/小写后为空、超 16 字符或含白名单外字符）。
	ErrInvalidOpenWithExt = errors.New("invalid extension")
	// ErrInvalidOpenWithOpener 表示 opener 不在枚举白名单内。
	ErrInvalidOpenWithOpener = errors.New("invalid opener")
)

// NormalizeOpenWithExt 规范化扩展名：去首尾空白与前置点、转小写，
// 须为 1..16 个 [a-z0-9] 字符，否则返回 ErrInvalidOpenWithExt。
func NormalizeOpenWithExt(ext string) (string, error) {
	ext = strings.ToLower(strings.TrimSpace(ext))
	ext = strings.TrimLeft(ext, ".")
	if ext == "" || len(ext) > 16 {
		return "", ErrInvalidOpenWithExt
	}
	for i := 0; i < len(ext); i++ {
		c := ext[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "", ErrInvalidOpenWithExt
		}
	}
	return ext, nil
}

// ValidateOpenWithOpener 校验 opener 枚举（office/drawio/excalidraw/text/
// markdown/code/web/default）。
func ValidateOpenWithOpener(opener string) error {
	if !openWithOpeners[opener] {
		return ErrInvalidOpenWithOpener
	}
	return nil
}

// ListOpenWith 返回用户的全部打开方式偏好（按 ext 排序；无记录返回空切片）。
func (s *UserStore) ListOpenWith(userID uuid.UUID) ([]OpenWithPreference, error) {
	var rows []OpenWithPreference
	if err := s.db.Raw(
		"SELECT ext, opener, updated_at FROM user_open_with WHERE user_id = ? ORDER BY ext",
		userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []OpenWithPreference{}
	}
	return rows, nil
}

// SetOpenWith upsert 一条偏好（ext/opener 须先经 NormalizeOpenWithExt /
// ValidateOpenWithOpener 校验），返回落库后的记录。
func (s *UserStore) SetOpenWith(userID uuid.UUID, ext, opener string) (OpenWithPreference, error) {
	now := time.Now().UTC()
	if err := s.db.Exec(`
		INSERT INTO user_open_with (user_id, ext, opener, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, ext)
		DO UPDATE SET opener = EXCLUDED.opener, updated_at = EXCLUDED.updated_at`,
		userID, ext, opener, now).Error; err != nil {
		return OpenWithPreference{}, err
	}
	return OpenWithPreference{Ext: ext, Opener: opener, UpdatedAt: now}, nil
}

// DeleteOpenWith 删除一条偏好（幂等：记录不存在时不报错）。
func (s *UserStore) DeleteOpenWith(userID uuid.UUID, ext string) error {
	return s.db.Exec("DELETE FROM user_open_with WHERE user_id = ? AND ext = ?", userID, ext).Error
}
