package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/collab"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// fakeCollabFiles 是 collabFileAuthorizer 的内存实现（模式同 rawFakeTree）：
// 维护「文件 → owner/额外写者」的最小文件树，语义对齐 files.Store 的
// ValidateReplaceTarget——不存在/非文件 404 语义（ErrNotFound/ErrInvalidTarget）、
// 无写权限 ErrForbidden（owner 或显式写者放行）。
type fakeCollabFiles struct {
	owners  map[uuid.UUID]uuid.UUID          // fileID → owner
	writers map[uuid.UUID]map[uuid.UUID]bool // fileID → 额外写者
	folders map[uuid.UUID]bool               // 目录（ErrInvalidTarget）
}

func (f *fakeCollabFiles) ValidateReplaceTarget(user, target uuid.UUID) (files.File, error) {
	if f.folders[target] {
		return files.File{}, files.ErrInvalidTarget
	}
	owner, ok := f.owners[target]
	if !ok {
		return files.File{}, files.ErrNotFound
	}
	if owner != user && !f.writers[target][user] {
		return files.File{}, files.ErrForbidden
	}
	return files.File{ID: target, OwnerID: owner, Type: "file"}, nil
}

// collabTestEnv 构造协作 WS 端点路由（模式同 aclTestEnv：内存依赖直注）。
func collabTestEnv(t *testing.T) (*gin.Engine, *Handler, *fakeCollabFiles) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	fake := &fakeCollabFiles{
		owners:  map[uuid.UUID]uuid.UUID{},
		writers: map[uuid.UUID]map[uuid.UUID]bool{},
		folders: map[uuid.UUID]bool{},
	}
	h.collab = collab.NewManager()
	h.collabFiles = fake
	h.wsSecret = "collab-test-secret"
	router := gin.New()
	router.GET("/api/v1/collab/:fileId/ws", h.wsCollab)
	return router, h, fake
}

func collabToken(t *testing.T, h *Handler, uid uuid.UUID) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   uid.String(),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	s, err := tok.SignedString([]byte(h.wsSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func callCollabWS(router *gin.Engine, fileID uuid.UUID, protocols ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/collab/"+fileID.String()+"/ws", nil)
	if len(protocols) > 0 {
		req.Header.Set("Sec-WebSocket-Protocol", strings.Join(protocols, ", "))
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestCollabWSRequiresToken 无 token（子协议缺失）→ 401。
func TestCollabWSRequiresToken(t *testing.T) {
	router, _, _ := collabTestEnv(t)
	if w := callCollabWS(router, uuid.New()); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", w.Code)
	}
}

// TestCollabWSRejectsForgedToken 伪造 token → 401。
func TestCollabWSRejectsForgedToken(t *testing.T) {
	router, _, fake := collabTestEnv(t)
	owner := uuid.New()
	fileID := uuid.New()
	fake.owners[fileID] = owner
	if w := callCollabWS(router, fileID, "bearer", "forged.token.value"); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged token: status = %d, want 401", w.Code)
	}
}

// TestCollabWSDisabledBySetting collab.enabled=false → 404（且不创建房间）。
func TestCollabWSDisabledBySetting(t *testing.T) {
	router, h, fake := collabTestEnv(t)
	h.settings = &fakeSettingsService{boolKeys: map[string]bool{settings.KeyCollabEnabled: false}}
	owner := uuid.New()
	fileID := uuid.New()
	fake.owners[fileID] = owner
	w := callCollabWS(router, fileID, "bearer", collabToken(t, h, owner))
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled: status = %d, want 404", w.Code)
	}
	if h.collab.RoomCount() != 0 {
		t.Fatalf("禁用时不应创建房间：room count = %d", h.collab.RoomCount())
	}
}

// TestCollabWSWritePermission 文件无写权限 → 403；不存在/目录 → 404；
// owner/额外写者放行（进入升级阶段——recorder 不支持 hijack，升级失败
// 返回非 401/403/404 即视为权限校验通过）。
func TestCollabWSWritePermission(t *testing.T) {
	router, h, fake := collabTestEnv(t)
	owner, writer, stranger := uuid.New(), uuid.New(), uuid.New()
	fileID, folderID, missing := uuid.New(), uuid.New(), uuid.New()
	fake.owners[fileID] = owner
	fake.writers[fileID] = map[uuid.UUID]bool{writer: true}
	fake.folders[folderID] = true

	if w := callCollabWS(router, fileID, "bearer", collabToken(t, h, stranger)); w.Code != http.StatusForbidden {
		t.Fatalf("stranger: status = %d, want 403", w.Code)
	}
	if w := callCollabWS(router, missing, "bearer", collabToken(t, h, owner)); w.Code != http.StatusNotFound {
		t.Fatalf("missing file: status = %d, want 404", w.Code)
	}
	if w := callCollabWS(router, folderID, "bearer", collabToken(t, h, owner)); w.Code != http.StatusNotFound {
		t.Fatalf("folder target: status = %d, want 404", w.Code)
	}
	if h.collab.RoomCount() != 0 {
		t.Fatalf("权限校验失败不应创建房间：room count = %d", h.collab.RoomCount())
	}
	// owner 与额外写者通过权限校验（进入升级；ResponseRecorder 无法完成
	// WebSocket 升级，非 401/403/404 即证明未被权限层拦截）。
	for _, uid := range []uuid.UUID{owner, writer} {
		if w := callCollabWS(router, fileID, "bearer", collabToken(t, h, uid)); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
			t.Fatalf("user %s: 意外被拦截 status = %d", uid, w.Code)
		}
	}
}

// TestCollabWSEndToEnd 真实 WebSocket 全链路：JWT 子协议连接 → join → init
// 快照 → 第二人加入上线通告 → steps 权威排序广播（含发送者回环）→
// leave 通告与空房清理。
func TestCollabWSEndToEnd(t *testing.T) {
	_, h, fake := collabTestEnv(t)
	alice, bob := uuid.New(), uuid.New()
	fileID := uuid.New()
	fake.owners[fileID] = alice
	fake.writers[fileID] = map[uuid.UUID]bool{bob: true}

	router := gin.New()
	router.GET("/api/v1/collab/:fileId/ws", h.wsCollab)
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/collab/" + fileID.String() + "/ws"

	dial := func(uid uuid.UUID) *websocket.Conn {
		t.Helper()
		header := http.Header{}
		header.Set("Sec-WebSocket-Protocol", "bearer, "+collabToken(t, h, uid))
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
		if err != nil {
			t.Fatalf("dial as %s: %v", uid, err)
		}
		return conn
	}
	readMsg := func(conn *websocket.Conn) map[string]any {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return m
	}
	writeMsg := func(conn *websocket.Conn, payload string) {
		t.Helper()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatalf("write %s: %v", payload, err)
		}
	}

	aliceConn := dial(alice)
	defer aliceConn.Close()
	writeMsg(aliceConn, `{"type":"join","name":"Alice","color":"#ff0000"}`)
	init := readMsg(aliceConn)
	if init["type"] != "init" || init["version"].(float64) != 0 {
		t.Fatalf("alice init: %v", init)
	}
	if parts := init["participants"].([]any); len(parts) != 1 || parts[0].(map[string]any)["leader"] != true {
		t.Fatalf("alice init participants: %v", init["participants"])
	}

	bobConn := dial(bob)
	defer bobConn.Close()
	writeMsg(bobConn, `{"type":"join","name":"Bob","color":"#00ff00"}`)
	// Alice（leader）先收 sync-begin + sync-request(target=bob)。
	if msg := readMsg(aliceConn); msg["type"] != "sync-begin" {
		t.Fatalf("alice sync-begin: %v", msg)
	}
	req := readMsg(aliceConn)
	if req["type"] != "sync-request" {
		t.Fatalf("alice sync-request: %v", req)
	}
	target, _ := req["target"].(string)
	if target == "" {
		t.Fatalf("sync-request 缺 target: %v", req)
	}
	// syncing 期间 leader 在途冲账 steps 放行：广播回环（version=1）。
	writeMsg(aliceConn, `{"type":"steps","steps":[{"stepType":"replace","from":0}],"clientID":7,"version":0}`)
	echo := readMsg(aliceConn)
	if echo["type"] != "steps" || echo["version"].(float64) != 1 {
		t.Fatalf("leader syncing 期间 steps 回环: %v", echo)
	}
	// 以落后版本上报快照 → 非致命 error（stale snapshot）+ 重发 sync-request
	//（两者分属写泵的 out/room 通道，到达顺序不确定，按集合断言）。
	writeMsg(aliceConn, fmt.Sprintf(`{"type":"snapshot","doc":{"type":"doc"},"version":0,"target":%q}`, target))
	staleMsgs := []map[string]any{readMsg(aliceConn), readMsg(aliceConn)}
	hasErr, hasReq := false, false
	for _, msg := range staleMsgs {
		if msg["type"] == "error" && msg["message"] == "stale snapshot" {
			hasErr = true
		}
		if msg["type"] == "sync-request" && msg["target"] == target {
			hasReq = true
		}
	}
	if !hasErr || !hasReq {
		t.Fatalf("stale snapshot 应答错误: %v", staleMsgs)
	}
	// 追平权威版本重报 → Bob 以 init-doc（leader 实时文档 + version）转正。
	writeMsg(aliceConn, fmt.Sprintf(`{"type":"snapshot","doc":{"type":"doc","hello":true},"version":1,"target":%q}`, target))
	bobInit := readMsg(bobConn)
	if bobInit["type"] != "init-doc" || bobInit["version"].(float64) != 1 {
		t.Fatalf("bob init-doc: %v", bobInit)
	}
	if doc, ok := bobInit["doc"].(map[string]any); !ok || doc["hello"] != true {
		t.Fatalf("bob init-doc doc: %v", bobInit["doc"])
	}
	if parts := bobInit["participants"].([]any); len(parts) != 2 {
		t.Fatalf("bob init-doc participants: %v", bobInit["participants"])
	}
	// Alice 收到 Bob 上线通告（presence）与 sync-end 恢复。
	presence := readMsg(aliceConn)
	if presence["type"] != "presence" || presence["name"] != "Bob" || presence["leader"] != false {
		t.Fatalf("alice sees bob presence: %v", presence)
	}
	if msg := readMsg(aliceConn); msg["type"] != "sync-end" || msg["version"].(float64) != 1 {
		t.Fatalf("alice sync-end: %v", msg)
	}

	// 双人均为在册成员后，Alice 提交 2 步：双方收到同一广播（version=3，
	// clientIDs 透传为数值）。
	writeMsg(aliceConn, `{"type":"steps","steps":[{"stepType":"replace","from":1},{"stepType":"replace","from":2}],"clientID":42,"version":1}`)
	for name, conn := range map[string]*websocket.Conn{"alice": aliceConn, "bob": bobConn} {
		broadcast := readMsg(conn)
		if broadcast["type"] != "steps" || broadcast["version"].(float64) != 3 {
			t.Fatalf("%s steps broadcast: %v", name, broadcast)
		}
		if steps := broadcast["steps"].([]any); len(steps) != 2 {
			t.Fatalf("%s steps len = %d, want 2", name, len(steps))
		}
		if ids := broadcast["clientIDs"].([]any); len(ids) != 2 || ids[0].(float64) != 42 {
			t.Fatalf("%s clientIDs: %v", name, ids)
		}
	}

	// 未知类型回 error；ping 为静默保活（不产生下行）。
	writeMsg(bobConn, `{"type":"bogus"}`)
	errMsg := readMsg(bobConn)
	if errMsg["type"] != "error" {
		t.Fatalf("unknown type reply: %v", errMsg)
	}
	writeMsg(bobConn, `{"type":"ping"}`)

	// Bob 关闭连接：Alice 收到 leave；空房自动清理。
	_ = bobConn.Close()
	leave := readMsg(aliceConn)
	if leave["type"] != "leave" {
		t.Fatalf("alice leave notice: %v", leave)
	}
	_ = aliceConn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for h.collab.RoomCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.collab.RoomCount() != 0 {
		t.Fatalf("空房未清理：room count = %d", h.collab.RoomCount())
	}
}
