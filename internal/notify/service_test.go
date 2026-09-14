package notify

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestService() (*Service, *MemoryStore, *MemoryPreferenceRepo) {
	store := NewMemoryStore()
	prefs := NewMemoryPreferenceRepo()
	svc := NewService(store, prefs)
	return svc, store, prefs
}

// 偏好短路：无记录 = 默认开启（落库）；disabled 跳过（不落库、不报错）；
// 重新开启后恢复落库。
func TestNotifyPreferenceShortCircuit(t *testing.T) {
	svc, store, prefs := newTestService()
	user := uuid.New()

	// 无偏好记录：默认开启，通知落库；resourceID=uuid.Nil 存 NULL。
	if err := svc.Notify(user, EventUploadCompleted, "上传完成：a.txt", "body", uuid.Nil); err != nil {
		t.Fatal(err)
	}
	items, _ := store.List(user, false, 10, "")
	if len(items) != 1 {
		t.Fatalf("default enabled: items = %d, want 1", len(items))
	}
	if items[0].ResourceID != nil {
		t.Fatalf("uuid.Nil resource must be stored as NULL, got %v", items[0].ResourceID)
	}

	// 关闭偏好：跳过（返回 nil），不产生任何行。
	if err := prefs.Set(user, EventUploadCompleted, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Notify(user, EventUploadCompleted, "t", "b", uuid.Nil); err != nil {
		t.Fatalf("disabled preference must short-circuit silently, got %v", err)
	}
	items, _ = store.List(user, false, 10, "")
	if len(items) != 1 {
		t.Fatalf("disabled: items = %d, want 1 (no new rows)", len(items))
	}

	// 其他事件类型不受影响。
	if err := svc.Notify(user, EventShareAccessed, "t", "b", uuid.New()); err != nil {
		t.Fatal(err)
	}
	items, _ = store.List(user, false, 10, "")
	if len(items) != 2 {
		t.Fatalf("other event type: items = %d, want 2", len(items))
	}

	// 重新开启：恢复落库。
	if err := prefs.Set(user, EventUploadCompleted, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Notify(user, EventUploadCompleted, "t", "b", uuid.Nil); err != nil {
		t.Fatal(err)
	}
	items, _ = store.List(user, false, 10, "")
	if len(items) != 3 {
		t.Fatalf("re-enabled: items = %d, want 3", len(items))
	}
}

// 出站渠道（webhook/邮件）与站内通知共用同一偏好开关：偏好关闭时
// 两者一并短路；开启时均在落库成功后触发且参数一致。
func TestOutboundChannelsSharePreference(t *testing.T) {
	svc, store, prefs := newTestService()
	user := uuid.New()

	var mails, hooks int
	var hookEvent, hookTitle, mailTitle string
	var hookResource uuid.UUID
	svc.SetMailNotifier(func(uid uuid.UUID, eventType, title, body string) {
		mails++
		mailTitle = title
	})
	svc.SetWebhookEnqueuer(func(uid uuid.UUID, eventType, title, body string, resourceID uuid.UUID) {
		hooks++
		hookEvent, hookTitle = eventType, title
		hookResource = resourceID
	})

	// 偏好关闭：站内不落库，邮件与 webhook 均不触发。
	if err := prefs.Set(user, EventUploadCompleted, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Notify(user, EventUploadCompleted, "上传完成：a.txt", "body", uuid.New()); err != nil {
		t.Fatalf("disabled preference must short-circuit silently, got %v", err)
	}
	if items, _ := store.List(user, false, 10, ""); len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
	if mails != 0 || hooks != 0 {
		t.Fatalf("disabled preference must skip both channels: mails=%d hooks=%d", mails, hooks)
	}

	// 偏好开启（重新启用）：站内落库 + 两个渠道均触发，参数一致。
	resource := uuid.New()
	if err := prefs.Set(user, EventUploadCompleted, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Notify(user, EventUploadCompleted, "上传完成：a.txt", "body", resource); err != nil {
		t.Fatal(err)
	}
	if items, _ := store.List(user, false, 10, ""); len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if mails != 1 || hooks != 1 {
		t.Fatalf("enabled preference must trigger both channels: mails=%d hooks=%d", mails, hooks)
	}
	if mailTitle != "上传完成：a.txt" || hookEvent != EventUploadCompleted || hookTitle != "上传完成：a.txt" || hookResource != resource {
		t.Fatalf("channel args mismatch: mailTitle=%q hookEvent=%q hookResource=%s", mailTitle, hookEvent, hookResource)
	}

	// 未注入渠道（nil）：仅站内落库，无 panic（防御默认态）。
	svc2, _, _ := newTestService()
	if err := svc2.Notify(user, EventShareAccessed, "t", "b", uuid.Nil); err != nil {
		t.Fatalf("no channels injected: %v", err)
	}
}

// NotifyMany：逐个偏好短路（disabled 用户跳过、其余落库）、去重、过滤零值 ID。
func TestNotifyManyPerUserPreferenceAndDedupe(t *testing.T) {
	svc, store, prefs := newTestService()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	if err := prefs.Set(b, EventFileUpdated, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.NotifyMany([]uuid.UUID{a, b, c, a, uuid.Nil}, EventFileUpdated, "团队文件已更新：x.doc", "body", uuid.New()); err != nil {
		t.Fatal(err)
	}
	// a、c 各 1 条；b 被偏好短路；重复 a 与零值 ID 不重复落库。
	for _, u := range []uuid.UUID{a, c} {
		items, _ := store.List(u, false, 10, "")
		if len(items) != 1 {
			t.Fatalf("user %s: items = %d, want 1", u, len(items))
		}
	}
	items, _ := store.List(b, false, 10, "")
	if len(items) != 0 {
		t.Fatalf("disabled user b: items = %d, want 0", len(items))
	}
}

// 偏好读写：SetPreference 未知类型报错；生效值读取（无记录=默认）。
func TestPreferenceGetSetValidation(t *testing.T) {
	svc, _, _ := newTestService()
	user := uuid.New()

	enabled, err := svc.PreferenceEnabled(user, EventShareAccessed)
	if err != nil || !enabled {
		t.Fatalf("no record: enabled=%v err=%v, want default true", enabled, err)
	}
	if err := svc.SetPreference(user, EventShareAccessed, false); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := svc.PreferenceEnabled(user, EventShareAccessed); enabled {
		t.Fatal("after disable: enabled must be false")
	}
	if err := svc.SetPreference(user, "not.an.event", true); err == nil {
		t.Fatal("unknown event type must be rejected")
	}
}

// 列表游标：created_at 倒序；cursor 取该时间点（不含）之前的记录；
// 非法 cursor 报 ErrInvalidCursor。
func TestListCursorPagination(t *testing.T) {
	store := NewMemoryStore()
	user := uuid.New()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := store.Create(Notification{ID: uuid.New(), UserID: user, Type: EventUploadCompleted, Title: "n", CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	// 第一页（limit 2）：最新两条。
	page1, err := store.List(user, false, 2, "")
	if err != nil || len(page1) != 2 {
		t.Fatalf("page1 = %d items, err=%v, want 2", len(page1), err)
	}
	if page1[0].CreatedAt.Before(page1[1].CreatedAt) {
		t.Fatal("list must be created_at desc")
	}
	// 游标 = 第一页最后一条的 created_at → 下一页不含该条。
	cursor := page1[len(page1)-1].CreatedAt.UTC().Format(time.RFC3339Nano)
	page2, err := store.List(user, false, 2, cursor)
	if err != nil || len(page2) != 2 {
		t.Fatalf("page2 = %d items, err=%v, want 2", len(page2), err)
	}
	if !page2[0].CreatedAt.Before(page1[len(page1)-1].CreatedAt) {
		t.Fatalf("page2 must be strictly older than cursor, got %v", page2[0].CreatedAt)
	}
	// 最后一页：余量 1 条。
	cursor2 := page2[len(page2)-1].CreatedAt.UTC().Format(time.RFC3339Nano)
	page3, err := store.List(user, false, 2, cursor2)
	if err != nil || len(page3) != 1 {
		t.Fatalf("page3 = %d items, err=%v, want 1", len(page3), err)
	}
	// 非法游标。
	if _, err := store.List(user, false, 2, "not-a-time"); err == nil {
		t.Fatal("invalid cursor must error")
	}
}

// MarkRead 归属：只能标记自己的通知；他人通知返回 false；幂等重复标记。
func TestMarkReadOwnershipAndIdempotency(t *testing.T) {
	store := NewMemoryStore()
	owner, other := uuid.New(), uuid.New()
	n := Notification{ID: uuid.New(), UserID: owner, Type: EventShareAccessed, Title: "t", CreatedAt: time.Now()}
	if err := store.Create(n); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.MarkRead(other, n.ID); ok {
		t.Fatal("other user must not mark foreign notification as read")
	}
	if ok, err := store.MarkRead(owner, n.ID); err != nil || !ok {
		t.Fatalf("owner mark read: ok=%v err=%v", ok, err)
	}
	// 幂等：重复标记仍返回 true。
	if ok, _ := store.MarkRead(owner, n.ID); !ok {
		t.Fatal("repeated mark read must be idempotent true")
	}
	items, _ := store.List(owner, false, 10, "")
	if len(items) != 1 || !items[0].IsRead || items[0].ReadAt == nil {
		t.Fatalf("after mark read: %+v", items)
	}
	// 不存在的 ID。
	if ok, _ := store.MarkRead(owner, uuid.New()); ok {
		t.Fatal("nonexistent notification must return false")
	}
}

// CountUnread / MarkAllRead / unread_only 过滤。
func TestCountUnreadMarkAllAndUnreadOnly(t *testing.T) {
	svc, store, _ := newTestService()
	user := uuid.New()
	other := uuid.New()
	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := store.Create(Notification{ID: uuid.New(), UserID: user, Type: EventUploadCompleted, Title: "t", CreatedAt: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Create(Notification{ID: uuid.New(), UserID: other, Type: EventUploadCompleted, Title: "t", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.CountUnread(user); n != 3 {
		t.Fatalf("unread = %d, want 3", n)
	}
	if n, _ := svc.MarkAllRead(user); n != 3 {
		t.Fatalf("mark all read affected = %d, want 3", n)
	}
	if n, _ := svc.CountUnread(user); n != 0 {
		t.Fatalf("after mark all: unread = %d, want 0", n)
	}
	if n, _ := svc.CountUnread(other); n != 1 {
		t.Fatalf("other user unread = %d, want 1 (MarkAllRead is per-user)", n)
	}
	if items, _ := svc.List(user, true, 10, ""); len(items) != 0 {
		t.Fatalf("unread_only after mark all = %d, want 0", len(items))
	}
}
