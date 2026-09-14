package share

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// shareNotifyCall 记录一次通知回调的参数。
type shareNotifyCall struct {
	user     uuid.UUID
	event    string
	title    string
	resource uuid.UUID
}

// 公开分享下载成功：通知 owner share.accessed（标题含文件名，资源为文件 ID）；
// 预览（ResolveForPreview）与元数据解析不通知。
func TestPublicDownloadNotifiesOwner(t *testing.T) {
	svc, _, _, owner, fileID, _ := newTestService()
	sh, token, err := svc.Create(owner, fileID, PermissionDownload, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls []shareNotifyCall
	svc.SetNotifyDispatcher(func(userID uuid.UUID, eventType, title, body string, resourceID uuid.UUID) {
		calls = append(calls, shareNotifyCall{user: userID, event: eventType, title: title, resource: resourceID})
	})
	// 预览不通知。
	if _, err := svc.ResolveForPreview(token); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("preview must not notify: %+v", calls)
	}
	// 下载成功通知 owner。
	if _, err := svc.ResolveForDownload(token); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.user != owner || c.event != "share.accessed" || c.resource != sh.FileID {
		t.Fatalf("call = %+v, want owner=%s share.accessed resource=%s", c, owner, sh.FileID)
	}
	if !strings.Contains(c.title, "report.txt") {
		t.Fatalf("title must contain file name: %q", c.title)
	}
}

// 私有分享下载：被授权用户下载通知 owner；owner 本人下载不通知（避免噪音）。
func TestPrivateDownloadNotifiesOwnerExceptSelf(t *testing.T) {
	svc, _, _, owner, fileID, _ := newTestService()
	grantee := uuid.New()
	sh, err := svc.CreatePrivate(owner, fileID, PermissionDownload, 0, nil, []uuid.UUID{grantee}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	var lastUser uuid.UUID
	svc.SetNotifyDispatcher(func(userID uuid.UUID, eventType, title, body string, resourceID uuid.UUID) {
		calls++
		lastUser = userID
	})
	// owner 本人下载：不通知。
	if _, err := svc.ResolveForUserForDownload(sh.ID, fileID, owner); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("owner self-download must not notify, calls = %d", calls)
	}
	// 被授权用户下载：通知 owner。
	if _, err := svc.ResolveForUserForDownload(sh.ID, fileID, grantee); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || lastUser != owner {
		t.Fatalf("calls = %d lastUser = %s, want 1 notification to owner %s", calls, lastUser, owner)
	}
}
