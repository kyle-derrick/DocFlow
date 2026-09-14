package share

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

func durPtr(d time.Duration) *time.Duration { return &d }

func intPtrValue(v int) *int { return &v }

// newPasswordTestEnv 构造带可控时钟的分享服务与已注册的可用文件，
// 返回 (svc, repo, owner, fileID)。
func newPasswordTestEnv(t *testing.T) (*Service, *MemoryStore, uuid.UUID, uuid.UUID) {
	t.Helper()
	owner := uuid.New()
	source := newFakeFiles()
	fileID := source.addFile(owner, "secret.txt", "file", files.BlobStatusAvailable)
	repo := NewMemoryStore()
	svc := NewService(repo, source)
	return svc, repo, owner, fileID
}

// 密码哈希/验证矩阵：正确密码、错误密码、空密码、长度非法（创建侧）、
// 同密码不同分享哈希不同（share_id 盐）。
func TestSharePasswordHashAndVerifyMatrix(t *testing.T) {
	svc, repo, owner, fileID := newPasswordTestEnv(t)
	_ = repo

	// 创建侧长度校验：3 字符与 65 字符均拒绝，4/64 字符接受。
	for _, bad := range []string{"abc", strings.Repeat("x", 65)} {
		if _, _, err := svc.CreatePublic(owner, fileID, PermissionDownload, 0, nil, ShareOptions{Password: bad}); err != ErrInvalidPassword {
			t.Fatalf("CreatePublic(password len=%d) err = %v, want ErrInvalidPassword", len(bad), err)
		}
	}
	sh, token, err := svc.CreatePublic(owner, fileID, PermissionDownload, 0, nil, ShareOptions{Password: "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.HasPassword() {
		t.Fatal("share with password must report HasPassword")
	}
	if sh.PasswordHash == "" || strings.Contains(sh.PasswordHash, "hunter2") {
		t.Fatal("password hash must be a non-empty digest, not the plaintext")
	}
	// share_id 盐：同密码不同分享 → 哈希不同（防跨分享彩虹比对）。
	other, _, err := svc.CreatePublic(owner, fileID, PermissionDownload, 0, nil, ShareOptions{Password: "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if other.PasswordHash == sh.PasswordHash {
		t.Fatal("same password on different shares must hash differently (share_id salt)")
	}

	// 验证矩阵：正确 → 会话值；错误/空 → ErrInvalidCredentials；
	// 不存在 token → ErrNotFound；无密码分享 → ErrPasswordNotSet。
	if _, err := svc.VerifySharePassword(token, "hunter2"); err != nil {
		t.Fatalf("correct password: %v", err)
	}
	for _, wrong := range []string{"wrong", "", "hunter"} {
		if _, err := svc.VerifySharePassword(token, wrong); err != ErrInvalidCredentials {
			t.Fatalf("VerifySharePassword(%q) err = %v, want ErrInvalidCredentials", wrong, err)
		}
	}
	if _, err := svc.VerifySharePassword("no-such-token", "hunter2"); err != ErrNotFound {
		t.Fatalf("unknown token err = %v, want ErrNotFound", err)
	}
	_, plainToken, err := svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifySharePassword(plainToken, "hunter2"); err != ErrPasswordNotSet {
		t.Fatalf("passwordless verify err = %v, want ErrPasswordNotSet", err)
	}

	// 私有分享不受密码影响：显式传入密码返回 ErrInvalidPassword。
	if _, err := svc.CreatePrivateWithOptions(owner, fileID, PermissionView, 0, nil, nil, nil, ShareOptions{Password: "hunter2"}); err != ErrInvalidPassword {
		t.Fatalf("CreatePrivateWithOptions(password) err = %v, want ErrInvalidPassword", err)
	}
}

// 会话有效期矩阵：验证成功后 1 小时内有效；过期失效；跨分享会话无效；
// 篡改 cookie 值无效；空值无效。
func TestShareAccessSessionValidity(t *testing.T) {
	svc, _, owner, fileID := newPasswordTestEnv(t)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	sh, token, err := svc.CreatePublic(owner, fileID, PermissionDownload, 0, nil, ShareOptions{Password: "pass1234"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := svc.VerifySharePassword(token, "pass1234")
	if err != nil {
		t.Fatal(err)
	}
	if !svc.ValidateShareSession(sh.ID, value) {
		t.Fatal("freshly issued session must validate")
	}
	// 差一秒过期。
	now = now.Add(time.Hour - time.Second)
	if !svc.ValidateShareSession(sh.ID, value) {
		t.Fatal("session must still be valid just before expiry")
	}
	now = now.Add(2 * time.Second)
	if svc.ValidateShareSession(sh.ID, value) {
		t.Fatal("expired session must not validate")
	}
	// 篡改值 / 空值 / 跨分享。
	if svc.ValidateShareSession(sh.ID, value+"x") {
		t.Fatal("tampered cookie value must not validate")
	}
	if svc.ValidateShareSession(sh.ID, "") {
		t.Fatal("empty cookie value must not validate")
	}
	otherShare, _, err := svc.CreatePublic(owner, fileID, PermissionDownload, 0, nil, ShareOptions{Password: "pass1234"})
	if err != nil {
		t.Fatal(err)
	}
	if svc.ValidateShareSession(otherShare.ID, value) {
		t.Fatal("session must not validate for a different share")
	}
}

// 水印默认值矩阵：无 provider 用内置默认；provider 提供默认；显式指定覆盖；
// 非法模板（超长/控制字符）拒绝。
func TestWatermarkDefaultsResolution(t *testing.T) {
	svc, _, owner, fileID := newPasswordTestEnv(t)

	// 内置默认：开启 + "{date} {name}"。
	sh, _, err := svc.CreatePublic(owner, fileID, PermissionView, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.WatermarkEnabled || sh.WatermarkTemplate() != DefaultWatermarkTemplate {
		t.Fatalf("builtin defaults = (%v, %q)", sh.WatermarkEnabled, sh.WatermarkTemplate())
	}

	// provider 默认（关闭 + 自定义模板）。
	svc.SetWatermarkDefaultsProvider(func() (bool, string) { return false, "{name} {date}" })
	sh, _, err = svc.CreatePublic(owner, fileID, PermissionView, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sh.WatermarkEnabled || sh.WatermarkTemplate() != "{name} {date}" {
		t.Fatalf("provider defaults = (%v, %q)", sh.WatermarkEnabled, sh.WatermarkTemplate())
	}

	// 显式指定覆盖默认。
	enabled := true
	custom := "{email} 于 {date} 查看"
	sh, _, err = svc.CreatePublic(owner, fileID, PermissionView, 0, nil, ShareOptions{WatermarkEnabled: &enabled, WatermarkText: &custom})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.WatermarkEnabled || sh.WatermarkTemplate() != custom {
		t.Fatalf("explicit watermark = (%v, %q)", sh.WatermarkEnabled, sh.WatermarkTemplate())
	}
	private, err := svc.CreatePrivateWithOptions(owner, fileID, PermissionView, 0, nil, nil, nil, ShareOptions{WatermarkEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if !private.WatermarkEnabled {
		t.Fatal("private share watermark explicit enable must apply")
	}

	// 非法模板：超长与控制字符。
	tooLong := strings.Repeat("水", MaxWatermarkTextLen+1)
	if _, _, err := svc.CreatePublic(owner, fileID, PermissionView, 0, nil, ShareOptions{WatermarkText: &tooLong}); err != ErrInvalidWatermarkText {
		t.Fatalf("overlong watermark err = %v, want ErrInvalidWatermarkText", err)
	}
	withCtrl := "bad\x00template"
	if _, _, err := svc.CreatePublic(owner, fileID, PermissionView, 0, nil, ShareOptions{WatermarkText: &withCtrl}); err != ErrInvalidWatermarkText {
		t.Fatalf("control-char watermark err = %v, want ErrInvalidWatermarkText", err)
	}
}

// RenderWatermark 占位符替换矩阵：{email}/{ip} → 脱敏 IP 前缀、{date}、{name}、
// 未知占位符原样、空模板回退默认、IPv6 前缀。
func TestRenderWatermark(t *testing.T) {
	now := time.Date(2026, 9, 14, 8, 30, 0, 0, time.UTC)
	if got := RenderWatermark("{date} {name}", "report.pdf", "203.0.113.9", now); got != "2026-09-14 report.pdf" {
		t.Fatalf("default template render = %q", got)
	}
	if got := RenderWatermark("{email} {name}", "a.txt", "198.51.100.7", now); got != "198.51.* a.txt" {
		t.Fatalf("{email} render = %q", got)
	}
	if got := RenderWatermark("{ip}/{name}", "b.txt", "2001:db8::1", now); got != "2001:db8:*/b.txt" {
		t.Fatalf("ipv6 {ip} render = %q", got)
	}
	if got := RenderWatermark("", "c.txt", "1.2.3.4", now); got != "2026-09-14 c.txt" {
		t.Fatalf("empty template fallback = %q", got)
	}
	if got := RenderWatermark("{unknown} {name}", "d.txt", "1.2.3.4", now); got != "{unknown} d.txt" {
		t.Fatalf("unknown placeholder must be kept: %q", got)
	}
	if got := RenderWatermark("{ip}", "e.txt", "", now); got != "unknown" {
		t.Fatalf("missing ip fallback = %q", got)
	}
}

// IPPrefix 脱敏矩阵。
func TestIPPrefix(t *testing.T) {
	cases := map[string]string{
		"203.0.113.9":  "203.0.*",
		"198.51.100.7": "198.51.*",
		"2001:db8::1":  "2001:db8:*",
		"":             "",
		"not-an-ip":    "",
	}
	for ip, want := range cases {
		if got := IPPrefix(ip); got != want {
			t.Fatalf("IPPrefix(%q) = %q, want %q", ip, got, want)
		}
	}
}

// 事件写入与统计聚合（内存 repo）：总数、独立访客（distinct ip_hash）、
// 最近 20 条倒序；跨分享事件互不干扰。
func TestAccessEventStatsAggregation(t *testing.T) {
	svc, repo, owner, fileID := newPasswordTestEnv(t)
	sh, _, err := svc.CreatePublic(owner, fileID, PermissionDownload, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := svc.CreatePublic(owner, fileID, PermissionDownload, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	// 25 条事件（2 个访客），外加 1 条属于其他分享。
	for i := 0; i < 25; i++ {
		hash := "h1"
		if i%2 == 1 {
			hash = "h2"
		}
		if err := svc.RecordAccessEvent(AccessEvent{FileID: sh.FileID, ShareID: sh.ID, Action: ActionDownload, IPHash: hash, IPPrefix: "203.0.*", UserAgent: "ua", CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.RecordAccessEvent(AccessEvent{FileID: other.FileID, ShareID: other.ID, Action: ActionPreview, IPHash: "h9", CreatedAt: base}); err != nil {
		t.Fatal(err)
	}

	stats, err := repo.ShareAccessStats(sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalAccess != 25 {
		t.Fatalf("total = %d, want 25", stats.TotalAccess)
	}
	if stats.UniqueVisitors != 2 {
		t.Fatalf("unique visitors = %d, want 2", stats.UniqueVisitors)
	}
	if len(stats.Recent) != 20 {
		t.Fatalf("recent = %d, want 20", len(stats.Recent))
	}
	// 倒序：最新一条在前。
	if last := stats.Recent[0]; !last.CreatedAt.Equal(base.Add(24 * time.Minute)) {
		t.Fatalf("most recent event time = %v, want %v", last.CreatedAt, base.Add(24*time.Minute))
	}
	// 其他分享只见自己的 1 条。
	otherStats, err := repo.ShareAccessStats(other.ID)
	if err != nil || otherStats.TotalAccess != 1 {
		t.Fatalf("other share stats = (%+v, %v)", otherStats, err)
	}
}

// PATCH 矩阵：改有效期/下载上限/水印；0 值清除限制；非 owner 404；
// 非法值 400；密码等不可改字段不受影响。
func TestUpdateSharePatch(t *testing.T) {
	svc, _, owner, fileID := newPasswordTestEnv(t)
	sh, _, err := svc.CreatePublic(owner, fileID, PermissionDownload, time.Hour, nil, ShareOptions{Password: "pass1234"})
	if err != nil {
		t.Fatal(err)
	}

	// 同时更新有效期与水印。
	enabled := false
	text := "内部资料 {name}"
	updated, err := svc.Update(owner, sh.ID, SharePatch{ExpiresIn: durPtr(2 * time.Hour), WatermarkEnabled: &enabled, WatermarkText: &text})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ExpiresAt == nil || updated.WatermarkEnabled || updated.WatermarkTemplate() != text {
		t.Fatalf("patched share = %+v", updated)
	}
	// 密码不受 PATCH 影响（不可改字段）。
	if !updated.HasPassword() {
		t.Fatal("password must survive patch (not patchable)")
	}

	// 0 值清除限制：永久 + 不限下载 + 恢复默认水印模板。
	zero := 0
	empty := ""
	updated, err = svc.Update(owner, sh.ID, SharePatch{ExpiresIn: durPtr(0), MaxDownloads: &zero, WatermarkText: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ExpiresAt != nil || updated.MaxDownloads != nil || updated.WatermarkText != nil {
		t.Fatalf("cleared limits = %+v", updated)
	}

	// 设置下载上限。
	limit := 3
	if updated, err = svc.Update(owner, sh.ID, SharePatch{MaxDownloads: &limit}); err != nil || updated.MaxDownloads == nil || *updated.MaxDownloads != 3 {
		t.Fatalf("max_downloads patch = (%+v, %v)", updated, err)
	}

	// 非 owner / 不存在 → ErrNotFound。
	if _, err := svc.Update(uuid.New(), sh.ID, SharePatch{ExpiresIn: durPtr(time.Hour)}); err != ErrNotFound {
		t.Fatalf("non-owner err = %v, want ErrNotFound", err)
	}
	// 非法值。
	if _, err := svc.Update(owner, sh.ID, SharePatch{MaxDownloads: intPtrValue(-1)}); err != ErrInvalidMaxDownloads {
		t.Fatalf("negative max err = %v, want ErrInvalidMaxDownloads", err)
	}
	if _, err := svc.Update(owner, sh.ID, SharePatch{ExpiresIn: durPtr(-time.Second)}); err != ErrInvalidExpiry {
		t.Fatalf("negative expiry err = %v, want ErrInvalidExpiry", err)
	}
	// 空补丁：返回当前值。
	same, err := svc.Update(owner, sh.ID, SharePatch{})
	if err != nil || same.ID != sh.ID {
		t.Fatalf("empty patch = (%+v, %v)", same, err)
	}
}

// GetDetail：owner 可见详情含文件名与统计；非 owner 404。
func TestGetShareDetail(t *testing.T) {
	svc, _, owner, fileID := newPasswordTestEnv(t)
	sh, _, err := svc.CreatePublic(owner, fileID, PermissionDownload, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordAccessEvent(AccessEvent{FileID: sh.FileID, ShareID: sh.ID, Action: ActionPreview, IPHash: "h1", IPPrefix: "203.0.*", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	detail, err := svc.GetDetail(owner, sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.FileName != "secret.txt" {
		t.Fatalf("file name = %q", detail.FileName)
	}
	if detail.Stats.TotalAccess != 1 || len(detail.Stats.Recent) != 1 {
		t.Fatalf("stats = %+v", detail.Stats)
	}
	if !detail.HasStats {
		t.Fatal("HasStats must be true when repo provides stats")
	}
	if _, err := svc.GetDetail(uuid.New(), sh.ID); err != ErrNotFound {
		t.Fatalf("non-owner detail err = %v, want ErrNotFound", err)
	}
}
