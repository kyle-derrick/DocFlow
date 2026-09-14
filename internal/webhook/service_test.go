package webhook

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/notify"
)

func newTestService() (*Service, *MemoryStore) {
	store := NewMemoryStore()
	return NewService(store), store
}

// URL 校验：拒绝非 http(s) 方案、空 host、userinfo；允许 localhost 与
// 内网地址（自托管场景，不强制公网）。
func TestValidateURL(t *testing.T) {
	valid := []string{
		"https://example.com/hook",
		"http://localhost:9000/webhook",
		"http://192.168.1.10:8080/callback",
		"https://hooks.internal/path?x=1",
	}
	for _, u := range valid {
		if err := ValidateURL(u); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want nil", u, err)
		}
	}
	invalid := []string{
		"",
		"   ",
		"not-a-url",
		"ftp://example.com/hook",
		"javascript:alert(1)",
		"http://",                            // 空 host
		"https://user:pass@example.com/hook", // userinfo（凭据不应出现在 URL）
		"http://user@example.com/hook",       // 仅用户名
	}
	for _, u := range invalid {
		if err := ValidateURL(u); err == nil {
			t.Errorf("ValidateURL(%q) = nil, want error", u)
		}
	}
	// 前后空白被剔除后仍可注册（Create TrimSpace）。
	if err := ValidateURL("  https://example.com/hook  "); err != nil {
		t.Errorf("ValidateURL with surrounding spaces = %v, want nil", err)
	}
}

// 事件白名单：空列表、未知类型拒绝；重复去重且保持顺序。
func TestValidateEvents(t *testing.T) {
	if _, err := ValidateEvents(nil); err == nil {
		t.Fatal("empty events must be rejected")
	}
	if _, err := ValidateEvents([]string{notify.EventUploadCompleted, "not.an.event"}); err == nil {
		t.Fatal("unknown event type must be rejected")
	}
	got, err := ValidateEvents([]string{
		notify.EventShareAccessed, notify.EventUploadCompleted, notify.EventShareAccessed,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{notify.EventShareAccessed, notify.EventUploadCompleted}
	if len(got) != len(want) {
		t.Fatalf("dedup = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedup order = %v, want %v", got, want)
		}
	}
}

// Create：secret 为 whsec_ + 43 字符 URL-safe base64 且仅本次返回；
// 列表不含 secret；(user_id,url) 重复返回 ErrDuplicateURL（跨用户不冲突）。
func TestCreateSecretAndDuplicate(t *testing.T) {
	svc, _ := newTestService()
	user, other := uuid.New(), uuid.New()

	hook, secret, err := svc.Create(user, "https://example.com/hook", []string{notify.EventUploadCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "whsec_") || len(secret) != len("whsec_")+43 {
		t.Fatalf("secret = %q (len %d), want whsec_+43", secret, len(secret))
	}
	if hook.Secret == "" || hook.Secret != secret {
		t.Fatal("created row must carry the returned secret")
	}
	if !hook.Enabled || hook.FailureCount != 0 || hook.LastStatus != nil {
		t.Fatalf("initial state = %+v, want enabled/no failures/no status", hook)
	}

	// 同一用户同一 URL：409 语义错误；不同用户同一 URL 允许。
	if _, _, err := svc.Create(user, "https://example.com/hook", []string{notify.EventFileUpdated}); err != ErrDuplicateURL {
		t.Fatalf("duplicate url err = %v, want ErrDuplicateURL", err)
	}
	if _, _, err := svc.Create(other, "https://example.com/hook", []string{notify.EventFileUpdated}); err != nil {
		t.Fatalf("same url for other user: %v", err)
	}

	// List 不含 secret（HTTP 层 webhookJSON 也不渲染 Secret 字段）。
	hooks, err := svc.List(user)
	if err != nil || len(hooks) != 1 {
		t.Fatalf("list = %d items, err=%v, want 1", len(hooks), err)
	}
	if hooks[0].Secret != secret {
		t.Fatal("store keeps secret for signing (明文存储取舍)")
	}
}

// HooksForEvent：只返回 enabled 且订阅了该事件的 hook。
func TestHooksForEventFiltering(t *testing.T) {
	svc, _ := newTestService()
	user := uuid.New()
	h1, _, err := svc.Create(user, "https://a.example/hook", []string{notify.EventUploadCompleted, notify.EventShareAccessed})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Create(user, "https://b.example/hook", []string{notify.EventFileUpdated}); err != nil {
		t.Fatal(err)
	}
	hooks, err := svc.HooksForEvent(user, notify.EventUploadCompleted)
	if err != nil || len(hooks) != 1 || hooks[0].ID != h1.ID {
		t.Fatalf("HooksForEvent(upload.completed) = %d hooks, err=%v, want 1 (%s)", len(hooks), err, h1.ID)
	}
	// 禁用后不再返回。
	if _, err := svc.Update(user, h1.ID, false); err != nil {
		t.Fatal(err)
	}
	hooks, err = svc.HooksForEvent(user, notify.EventUploadCompleted)
	if err != nil || len(hooks) != 0 {
		t.Fatalf("after disable: hooks = %d, err=%v, want 0", len(hooks), err)
	}
	// 未知事件类型返回空（防御）。
	if hooks, err := svc.HooksForEvent(user, "not.an.event"); err != nil || hooks != nil {
		t.Fatalf("unknown event: hooks=%v err=%v, want nil/nil", hooks, err)
	}
}

// Update/Delete 归属：非属主一律 ErrNotFound（不泄露存在性）。
func TestUpdateDeleteOwnership(t *testing.T) {
	svc, _ := newTestService()
	owner, other := uuid.New(), uuid.New()
	hook, _, err := svc.Create(owner, "https://example.com/hook", []string{notify.EventShareAccessed})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Update(other, hook.ID, false); err != ErrNotFound {
		t.Fatalf("foreign update err = %v, want ErrNotFound", err)
	}
	updated, err := svc.Update(owner, hook.ID, false)
	if err != nil || updated.Enabled {
		t.Fatalf("owner update = (%v, %v), want disabled", updated.Enabled, err)
	}

	if err := svc.Delete(other, hook.ID); err != ErrNotFound {
		t.Fatalf("foreign delete err = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(owner, hook.ID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if err := svc.Delete(owner, hook.ID); err != ErrNotFound {
		t.Fatalf("repeat delete err = %v, want ErrNotFound", err)
	}
}

// EventList 编解码往返（PostgreSQL text[] 字面量）。
func TestEventListRoundTrip(t *testing.T) {
	in := EventList{notify.EventUploadCompleted, notify.EventFileUpdated}
	value, err := in.Value()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := value.(string); !ok || got != `{"upload.completed","file.updated"}` {
		t.Fatalf("Value() = %v, want literal array", value)
	}
	var out EventList
	if err := out.Scan([]byte(value.(string))); err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) || out[0] != in[0] || out[1] != in[1] {
		t.Fatalf("Scan round-trip = %v, want %v", out, in)
	}
	var empty EventList
	if err := empty.Scan("{}"); err != nil || len(empty) != 0 {
		t.Fatalf("empty literal = %v, %v, want empty/nil", empty, err)
	}
	if err := out.Scan(nil); err != nil || out != nil {
		t.Fatalf("nil scan = %v, %v, want nil/nil", out, err)
	}
}
