package http

import (
	"encoding/xml"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/settings"
)

func TestDavPathRejectsTraversalAndOutsideRoot(t *testing.T) {
	for _, raw := range []string{"", "/dav", "/webdavx/file", "/webdav/../secret", "/webdav/a%5Cb"} {
		if got, ok := davPath(raw, "/webdav"); ok || got != "" {
			t.Fatalf("davPath(%q) = %q, %v; want rejection", raw, got, ok)
		}
	}
	if got, ok := davPath("/webdav/a%20b", "/webdav"); !ok || got != "a b" {
		t.Fatalf("davPath escaped segment = %q, %v", got, ok)
	}
}

func TestDestinationPathAcceptsAbsoluteURIOnlyOnRequestHost(t *testing.T) {
	r := httptest.NewRequest("MOVE", "https://example.test/webdav/src", nil)
	for _, raw := range []string{"", "https://evil.test/webdav/dst", "/outside/dst", "/webdav/../dst"} {
		if got, ok := destinationPath(raw, "/webdav", r); ok || got != "" {
			t.Fatalf("destinationPath(%q) = %q, %v; want rejection", raw, got, ok)
		}
	}
	for _, raw := range []string{"/webdav/dir/file", "https://example.test/webdav/dir/file"} {
		if got, ok := destinationPath(raw, "/webdav", r); !ok || got != "dir/file" {
			t.Fatalf("destinationPath(%q) = %q, %v", raw, got, ok)
		}
	}
}

func TestWebDAVTokenDTODoesNotExposeSecrets(t *testing.T) {
	body := `{"id":"x","name":"n","created_at":"2026-01-01T00:00:00Z"}`
	for _, forbidden := range []string{"token_hash", "user_id", "revoked_at"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("DTO contains forbidden field %q", forbidden)
		}
	}
}

// newDavLockoutEnv 构造仅装配锁定策略的 davHandler（不触 tokens/存储，
// davAuthFail/davAuthLocked/davAuthOK 均为纯内存路径）。
func newDavLockoutEnv(h *Handler) *davHandler {
	return &davHandler{h: h}
}

// 防爆破参数运行时化：settings 配置的阈值/时长优先于 env 基线，第 N 次
// 失败即锁定，锁定窗口按 settings 的分钟数；成功验证清零计数。
func TestDavAuthFailLockoutUsesSettings(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	// env 基线与 settings 值刻意不同，验证 settings 优先。
	h.SetLoginLockout(5, 15*time.Minute)
	h.SetSettingsService(&fakeSettingsService{intKeys: map[string]int{
		settings.KeyLoginMaxRetries:  2,
		settings.KeyLoginLockMinutes: 30,
	}})
	d := newDavLockoutEnv(h)
	const key = "alice|10.0.0.1"

	d.davAuthFail(key)
	if locked, _ := d.davAuthLocked(key); locked {
		t.Fatal("第 1 次失败不应锁定（settings 阈值 2）")
	}
	d.davAuthFail(key)
	locked, wait := d.davAuthLocked(key)
	if !locked {
		t.Fatal("第 2 次失败应锁定（settings 阈值 2）")
	}
	if wait <= 0 || wait > 30*time.Minute {
		t.Fatalf("锁定窗口 = %v, want (0, 30m]（settings 时长）", wait)
	}
	// 成功验证清零。
	d.davAuthOK(key)
	if locked, _ = d.davAuthLocked(key); locked {
		t.Fatal("成功验证应清零该键计数")
	}
}

// settings 未配置（键未入库 / settings 未装配）回落 env 基线
// （LOGIN_MAX_RETRIES / LOGIN_LOCK_MINUTES）。
func TestDavAuthFailLockoutFallsBackToEnv(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.SetLoginLockout(3, 10*time.Minute)
	d := newDavLockoutEnv(h)
	const key = "bob|10.0.0.2"

	for i := 0; i < 2; i++ {
		d.davAuthFail(key)
		if locked, _ := d.davAuthLocked(key); locked {
			t.Fatalf("第 %d 次失败不应锁定（env 阈值 3）", i+1)
		}
	}
	d.davAuthFail(key)
	if locked, _ := d.davAuthLocked(key); !locked {
		t.Fatal("第 3 次失败应锁定（env 阈值 3）")
	}

	// settings 装配但键未入库：同样回落 env。
	h.SetSettingsService(&fakeSettingsService{intKeys: map[string]int{}})
	d2 := newDavLockoutEnv(h)
	const key2 = "carol|10.0.0.3"
	for i := 0; i < 2; i++ {
		d2.davAuthFail(key2)
		if locked, _ := d2.davAuthLocked(key2); locked {
			t.Fatalf("第 %d 次失败不应锁定（键未入库回落 env 阈值 3）", i+1)
		}
	}
	d2.davAuthFail(key2)
	if locked, _ := d2.davAuthLocked(key2); !locked {
		t.Fatal("第 3 次失败应锁定（键未入库回落 env 阈值 3）")
	}
}

func TestDAVMultistatusXMLNamespaceAndCollection(t *testing.T) {
	m := davMultistatus{XMLName: xml.Name{Local: "D:multistatus"}, XMLNS: "DAV:", Responses: []davResponse{{
		Href: "/webdav/folder/",
		Propstat: davPropstat{
			Prop: davProp{
				ResourceType: &struct {
					Collection davCollection `xml:"D:collection"`
				}{},
				DisplayName: "folder",
			},
			Status: "HTTP/1.1 200 OK",
		},
	}}}
	body, err := xml.Marshal(m)
	if err != nil {
		t.Fatalf("marshal PROPFIND response: %v", err)
	}

	var decoded struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal PROPFIND response: %v\n%s", err, body)
	}
	if decoded.XMLName.Space != "DAV:" || decoded.XMLName.Local != "multistatus" {
		t.Fatalf("decoded root XML name = %#v, want DAV: multistatus", decoded.XMLName)
	}

	decoder := xml.NewDecoder(strings.NewReader(string(body)))
	seenResponse, seenCollection, seenDisplayName := false, false, false
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("parse PROPFIND response: %v", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Space != "DAV:" {
			t.Fatalf("element %q has namespace %q, want DAV:", start.Name.Local, start.Name.Space)
		}
		switch start.Name.Local {
		case "response":
			seenResponse = true
		case "collection":
			seenCollection = true
		case "displayname":
			seenDisplayName = true
		}
	}
	if !seenResponse || !seenCollection || !seenDisplayName {
		t.Fatalf("parsed response structure = response:%v collection:%v displayname:%v", seenResponse, seenCollection, seenDisplayName)
	}
}
