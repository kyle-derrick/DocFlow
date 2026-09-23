package collab

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recvN 从参与者通道按序读 n 条消息（超时 5s 失败）。
func recvN(t *testing.T, p *Participant, n int) [][]byte {
	t.Helper()
	msgs := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		select {
		case msg := <-p.Send():
			msgs = append(msgs, msg)
		case <-time.After(5 * time.Second):
			t.Fatalf("participant %s: 期待第 %d 条消息超时（已收 %d）", p.ConnID, i+1, len(msgs))
		}
	}
	return msgs
}

// stepsOf 构造 n 个带 marker 的透传 step（模拟 ProseMirror step JSON）。
func stepsOf(marker string, n int) []json.RawMessage {
	out := make([]json.RawMessage, n)
	for i := range out {
		out[i] = json.RawMessage(fmt.Sprintf(`{"marker":%q,"i":%d}`, marker, i))
	}
	return out
}

func rawClientID(v int) json.RawMessage { return json.RawMessage(fmt.Sprintf("%d", v)) }

// roomVersion 读房间当前权威版本（包内测试用）。
func roomVersion(t *testing.T, m *Manager, fileID uuid.UUID) int64 {
	t.Helper()
	m.mu.Lock()
	r := m.rooms[fileID]
	m.mu.Unlock()
	if r == nil {
		t.Fatal("room missing")
	}
	return r.Version()
}

// completeJoin 模拟 leader 应答快照握手：读 leader 通道的 sync-begin +
// sync-request(target=joiner)，以指定 doc 与房间当前权威版本完成
// CompleteSnapshot，返回 joiner 通道的首条消息（init-doc）。leader 与
// 既有成员通道上产生的 presence/sync-end 由调用方按需消费。
func completeJoin(t *testing.T, m *Manager, leader, joiner *Participant, fileID uuid.UUID, doc string) map[string]any {
	t.Helper()
	msgs := recvN(t, leader, 2)
	begin := decode(t, msgs[0])
	req := decode(t, msgs[1])
	if begin["type"] != "sync-begin" {
		t.Fatalf("leader 首条消息 = %v, want sync-begin", begin["type"])
	}
	if req["type"] != "sync-request" || req["target"] != joiner.ConnID.String() {
		t.Fatalf("sync-request 错误: %s", msgs[1])
	}
	if err := m.CompleteSnapshot(leader, json.RawMessage(doc), roomVersion(t, m, fileID), joiner.ConnID); err != nil {
		t.Fatalf("CompleteSnapshot: %v", err)
	}
	first := decode(t, recvN(t, joiner, 1)[0])
	if first["type"] != "init-doc" {
		t.Fatalf("joiner 首条消息 = %v, want init-doc", first["type"])
	}
	return first
}

// drainPresenceAndSyncEnd 消费转正后既有成员（leader 视角）应收到的
// presence(新成员) + sync-end 两条消息。
func drainPresenceAndSyncEnd(t *testing.T, p *Participant, joiner *Participant) {
	t.Helper()
	msgs := recvN(t, p, 2)
	pres := decode(t, msgs[0])
	end := decode(t, msgs[1])
	if pres["type"] != "presence" || pres["connId"] != joiner.ConnID.String() {
		t.Fatalf("presence 通告错误: %s", msgs[0])
	}
	if end["type"] != "sync-end" {
		t.Fatalf("期待 sync-end, got %v", end["type"])
	}
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

// TestVersionMonotonicEqualsStepCount 验证版本单调递增且恒等于累计收到的
// steps 总数（3 + 2 = 5），每条广播的 version 为该批最后一步的版本号。
func TestVersionMonotonicEqualsStepCount(t *testing.T) {
	m := NewManager()
	fileID := uuid.New()
	a, err := m.Join(fileID, uuid.New(), "A", "#111111")
	if err != nil {
		t.Fatal(err)
	}
	recvN(t, a, 1) // init
	b, err := m.Join(fileID, uuid.New(), "B", "#222222")
	if err != nil {
		t.Fatal(err)
	}
	completeJoin(t, m, a, b, fileID, `{"type":"doc"}`)
	drainPresenceAndSyncEnd(t, a, b)

	if err := m.SubmitSteps(a, stepsOf("a1", 3), rawClientID(11)); err != nil {
		t.Fatal(err)
	}
	if err := m.SubmitSteps(b, stepsOf("b1", 2), rawClientID(22)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Participant{a, b} {
		msgs := recvN(t, p, 2)
		first := decode(t, msgs[0])
		second := decode(t, msgs[1])
		if first["type"] != "steps" || second["type"] != "steps" {
			t.Fatalf("expect steps broadcasts, got %v / %v", first["type"], second["type"])
		}
		if v := first["version"].(float64); v != 3 {
			t.Fatalf("first broadcast version = %v, want 3", v)
		}
		if v := second["version"].(float64); v != 5 {
			t.Fatalf("second broadcast version = %v, want 5", v)
		}
	}
	m.mu.Lock()
	room := m.rooms[fileID]
	m.mu.Unlock()
	if room == nil || room.Version() != 5 {
		t.Fatalf("room version = %v, want 5", room)
	}
}

// TestBroadcastConsistentOrderAcrossParticipants 验证同一消息广播到全部
// 参与者且各参与者看到的字节序列完全一致。
func TestBroadcastConsistentOrderAcrossParticipants(t *testing.T) {
	m := NewManager()
	fileID := uuid.New()
	a, _ := m.Join(fileID, uuid.New(), "A", "#111111")
	recvN(t, a, 1) // init
	b, _ := m.Join(fileID, uuid.New(), "B", "#222222")
	completeJoin(t, m, a, b, fileID, `{"type":"doc"}`)
	drainPresenceAndSyncEnd(t, a, b)
	c, _ := m.Join(fileID, uuid.New(), "C", "#333333")
	// 第三人加入：a（leader）收 sync-begin + sync-request；b 收 sync-begin。
	reqMsgs := recvN(t, a, 2)
	if decode(t, reqMsgs[0])["type"] != "sync-begin" || decode(t, reqMsgs[1])["type"] != "sync-request" {
		t.Fatalf("leader 第三人加入通告错误: %v", reqMsgs)
	}
	if msg := decode(t, recvN(t, b, 1)[0]); msg["type"] != "sync-begin" {
		t.Fatalf("b 应收到 sync-begin: %v", msg)
	}
	if err := m.CompleteSnapshot(a, json.RawMessage(`{"type":"doc"}`), roomVersion(t, m, fileID), c.ConnID); err != nil {
		t.Fatal(err)
	}
	initDoc := decode(t, recvN(t, c, 1)[0])
	if initDoc["type"] != "init-doc" {
		t.Fatalf("c 首条消息 = %v, want init-doc", initDoc["type"])
	}
	drainPresenceAndSyncEnd(t, a, c)
	drainPresenceAndSyncEnd(t, b, c)

	_ = m.SubmitSteps(b, stepsOf("b", 1), rawClientID(2))
	_ = m.SubmitSteps(c, stepsOf("c", 2), rawClientID(3))
	_ = m.SubmitSteps(a, stepsOf("a", 1), rawClientID(1))

	aMsgs := recvN(t, a, 3)
	bMsgs := recvN(t, b, 3)
	cMsgs := recvN(t, c, 3)
	for i := 0; i < 3; i++ {
		if string(aMsgs[i]) != string(bMsgs[i]) || string(bMsgs[i]) != string(cMsgs[i]) {
			t.Fatalf("消息 %d 在参与者间不一致:\na=%s\nb=%s\nc=%s", i, aMsgs[i], bMsgs[i], cMsgs[i])
		}
		msg := decode(t, aMsgs[i])
		want := []float64{1, 3, 4}[i]
		if msg["version"].(float64) != want {
			t.Fatalf("消息 %d version = %v, want %v", i, msg["version"], want)
		}
	}
}

// TestLeaderTransferOnLeave 验证最早加入者为 leader、其离开后转移给下一位
// （leave 通告 + 新 leader 的 presence 通告），以及新加入者 init-doc 名单正确。
func TestLeaderTransferOnLeave(t *testing.T) {
	m := NewManager()
	fileID := uuid.New()
	a, _ := m.Join(fileID, uuid.New(), "A", "#111111")
	recvN(t, a, 1) // init
	b, _ := m.Join(fileID, uuid.New(), "B", "#222222")
	completeJoin(t, m, a, b, fileID, `{"type":"doc"}`)
	drainPresenceAndSyncEnd(t, a, b)
	if !a.leader || b.leader {
		t.Fatalf("初始 leader 判定错误：a=%v b=%v", a.leader, b.leader)
	}

	m.Leave(a)
	msgs := recvN(t, b, 2)
	leave := decode(t, msgs[0])
	if leave["type"] != "leave" || leave["connId"] != a.ConnID.String() {
		t.Fatalf("leave 通告错误: %s", msgs[0])
	}
	promoted := decode(t, msgs[1])
	if promoted["type"] != "presence" || promoted["connId"] != b.ConnID.String() || promoted["leader"] != true {
		t.Fatalf("leader 转移通告错误: %s", msgs[1])
	}

	// 新加入者经快照握手转正：init-doc 名单中 b 为 leader、c 为普通成员。
	c, _ := m.Join(fileID, uuid.New(), "C", "#333333")
	initDoc := completeJoin(t, m, b, c, fileID, `{"type":"doc2"}`)
	parts := initDoc["participants"].([]any)
	if len(parts) != 2 {
		t.Fatalf("init-doc 参与者数 = %d, want 2", len(parts))
	}
	first := parts[0].(map[string]any)
	second := parts[1].(map[string]any)
	if first["connId"] != b.ConnID.String() || first["leader"] != true {
		t.Fatalf("init-doc 名单顺序/leader 错误: %v", initDoc)
	}
	if second["connId"] != c.ConnID.String() || second["leader"] != false {
		t.Fatalf("init-doc 自身条目错误: %v", initDoc)
	}
	if docObj, ok := initDoc["doc"].(map[string]any); !ok || docObj["type"] != "doc2" {
		t.Fatalf("init-doc doc 错误: %v", initDoc["doc"])
	}
	drainPresenceAndSyncEnd(t, b, c)

	// 全员离开后空房自动清理。
	m.Leave(b)
	m.Leave(c)
	deadline := time.Now().Add(2 * time.Second)
	for m.RoomCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.RoomCount() != 0 {
		t.Fatalf("空房未清理：room count = %d", m.RoomCount())
	}
}

// TestRoomFullRejectsJoin 验证满员拒绝（上限可配置；pending 加入者计入）。
func TestRoomFullRejectsJoin(t *testing.T) {
	m := NewManager()
	m.SetMaxParticipants(2)
	fileID := uuid.New()
	if _, err := m.Join(fileID, uuid.New(), "A", "#1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(fileID, uuid.New(), "B", "#2"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(fileID, uuid.New(), "C", "#3"); err == nil {
		t.Fatal("满员加入应返回 ErrRoomFull")
	} else if err != ErrRoomFull {
		t.Fatalf("满员加入错误 = %v, want ErrRoomFull", err)
	}
	// 最后一名在册成员离开：房间清空连带摘除 pending、房间回收；随后 D
	// 以首成员身份重新加入。
	m.Leave(m2p(t, m, fileID, 0))
	deadline := time.Now().Add(2 * time.Second)
	for m.RoomCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.RoomCount() != 0 {
		t.Fatal("空房未清理")
	}
	if _, err := m.Join(fileID, uuid.New(), "D", "#4"); err != nil {
		t.Fatalf("房间回收后重新加入失败: %v", err)
	}
}

func m2p(t *testing.T, m *Manager, fileID uuid.UUID, idx int) *Participant {
	t.Helper()
	m.mu.Lock()
	r := m.rooms[fileID]
	m.mu.Unlock()
	if r == nil {
		t.Fatal("room missing")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.participants[idx]
}

// TestConcurrentSubmitSerializes 验证并发（含非 leader 普通成员）提交时，
// 每个批次在房间锁下原子串行化：所有参与者看到同一条消息序列，版本按批
// 递增不重复不回退，且单条消息内的 steps 恒来自同一批次（不交错错版）。
func TestConcurrentSubmitSerializes(t *testing.T) {
	const (
		workers   = 4
		perWorker = 15
		total     = workers * perWorker // 60 < sendBuffer(64)：全程不触发慢消费者摘除，断言确定性
	)
	m := NewManager()
	fileID := uuid.New()
	var parts []*Participant
	// 顺序建房（b/c 均经快照握手转正），避免 pending 与后续 Join 的
	// sync-begin 交错。
	for i := 0; i < 3; i++ {
		p, err := m.Join(fileID, uuid.New(), string(rune('A'+i)), "#111111")
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
		if i == 0 {
			recvN(t, parts[0], 1) // init
			continue
		}
		completeJoin(t, m, parts[0], p, fileID, `{"type":"doc"}`)
		for j := 0; j < i; j++ {
			if j > 0 {
				// 非 leader 既有成员：转正前先收到本批加入的 sync-begin。
				if msg := decode(t, recvN(t, parts[j], 1)[0]); msg["type"] != "sync-begin" {
					t.Fatalf("成员 %d 应先收 sync-begin: %v", j, msg)
				}
			}
			drainPresenceAndSyncEnd(t, parts[j], p)
		}
	}

	// 每个参与者一个并发收集器（与提交并发运行）；counts 与切片访问均由
	// collectedMu 保护（等待轮询与收集并发）。
	collected := make([][][]byte, len(parts))
	counts := make([]int, len(parts))
	var collectedMu sync.Mutex
	var collectWG sync.WaitGroup
	stop := make(chan struct{})
	for i := range parts {
		collectWG.Add(1)
		go func(i int) {
			defer collectWG.Done()
			for {
				select {
				case msg, ok := <-parts[i].Send():
					if !ok {
						return // 通道关闭（参与者被摘除）
					}
					collectedMu.Lock()
					collected[i] = append(collected[i], msg)
					counts[i]++
					collectedMu.Unlock()
				case <-stop:
					return
				}
			}
		}(i)
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			p := parts[w%len(parts)] // 非 leader 成员同样参与并发提交
			for i := 0; i < perWorker; i++ {
				marker := fmt.Sprintf("w%d-m%d", w, i)
				if err := m.SubmitSteps(p, stepsOf(marker, 1), rawClientID(w)); err != nil {
					t.Errorf("worker %d submit: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// 等待收集器收满 total 条（广播为非阻塞投递，提交完成后应已全部入队）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		collectedMu.Lock()
		done := true
		for i := range counts {
			if counts[i] < total {
				done = false
			}
		}
		collectedMu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	collectWG.Wait()

	seenVersion := make(map[float64]bool)
	seenMarker := make(map[string]bool)
	for i := range parts {
		if len(collected[i]) != total {
			t.Fatalf("参与者 %d 收到 %d 条消息, want %d", i, len(collected[i]), total)
		}
		for j, raw := range collected[i] {
			msg := decode(t, raw)
			if msg["type"] != "steps" {
				t.Fatalf("参与者 %d 消息 %d 类型 = %v, want steps", i, j, msg["type"])
			}
			// 单条消息内 steps 恒为同一批次：marker 唯一且一致。
			steps := msg["steps"].([]any)
			if len(steps) != 1 {
				t.Fatalf("消息内 steps 数 = %d, want 1（批次被交错）", len(steps))
			}
			marker := steps[0].(map[string]any)["marker"].(string)
			if seenMarker[marker] && i == 0 {
				t.Fatalf("marker %s 重复出现（同一批次被广播两次）", marker)
			}
			seenMarker[marker] = true
			v := msg["version"].(float64)
			if v < 1 || v > total {
				t.Fatalf("version %v 越界", v)
			}
			if i == 0 {
				if seenVersion[v] {
					t.Fatalf("version %v 重复", v)
				}
				seenVersion[v] = true
			}
			if i > 0 && string(collected[i][j]) != string(collected[0][j]) {
				t.Fatalf("参与者 %d 消息 %d 与参与者 0 不一致（顺序错乱）", i, j)
			}
		}
	}
	// 每批 1 步：版本应为 1..total 的完整序列（即累计 steps 数）。
	if len(seenVersion) != total {
		t.Fatalf("版本序列长度 = %d, want %d", len(seenVersion), total)
	}
	m.mu.Lock()
	room := m.rooms[fileID]
	m.mu.Unlock()
	if room == nil || room.Version() != total {
		t.Fatalf("房间最终版本 = %v, want %d", room, total)
	}
}

// TestSubmitAfterRemoval 验证被摘除（如慢消费者）后提交返回 ErrNotInRoom。
func TestSubmitAfterRemoval(t *testing.T) {
	m := NewManager()
	fileID := uuid.New()
	a, _ := m.Join(fileID, uuid.New(), "A", "#1")
	m.Leave(a)
	if err := m.SubmitSteps(a, stepsOf("x", 1), rawClientID(1)); err != ErrNotInRoom {
		t.Fatalf("摘除后提交错误 = %v, want ErrNotInRoom", err)
	}
	if err := m.UpdatePresence(a, json.RawMessage(`{"anchor":1,"head":1}`)); err != ErrNotInRoom {
		t.Fatalf("摘除后 presence 错误 = %v, want ErrNotInRoom", err)
	}
}

// TestStaleSnapshotRetried 验证快照版本落后（上报时仍有在途 steps）时返回
// ErrStaleSnapshot 并向 leader 重发 sync-request；追平后同一 target 转正成功。
func TestStaleSnapshotRetried(t *testing.T) {
	m := NewManager()
	fileID := uuid.New()
	a, _ := m.Join(fileID, uuid.New(), "A", "#1")
	recvN(t, a, 1) // init
	b, _ := m.Join(fileID, uuid.New(), "B", "#2")
	// leader 通道：sync-begin + sync-request。
	recvN(t, a, 2)
	// syncing 期间 leader 提交在途冲账 steps（被放行）：版本 0 → 2。
	if err := m.SubmitSteps(a, stepsOf("a", 2), rawClientID(1)); err != nil {
		t.Fatal(err)
	}
	// 非 leader 视角不适用（b 尚未转正）。以落后版本上报 → stale + 重发请求。
	if err := m.CompleteSnapshot(a, json.RawMessage(`{"type":"doc"}`), 0, b.ConnID); err != ErrStaleSnapshot {
		t.Fatalf("过期快照错误 = %v, want ErrStaleSnapshot", err)
	}
	// leader 通道此刻为 [steps 回显(v2), 重发的 sync-request]（steps 广播
	// 先于 stale 重试投递）。
	echoMsgs := recvN(t, a, 2)
	stepsEcho := decode(t, echoMsgs[0])
	req := decode(t, echoMsgs[1])
	if stepsEcho["type"] != "steps" || stepsEcho["version"].(float64) != 2 {
		t.Fatalf("leader 应先收到自身 steps 回显: %v", stepsEcho)
	}
	if req["type"] != "sync-request" || req["target"] != b.ConnID.String() {
		t.Fatalf("应重发 sync-request: %v", req)
	}
	// 追平后以正确版本重报 → 转正成功。
	if err := m.CompleteSnapshot(a, json.RawMessage(`{"type":"doc","v":2}`), 2, b.ConnID); err != nil {
		t.Fatal(err)
	}
	initDoc := decode(t, recvN(t, b, 1)[0])
	if initDoc["type"] != "init-doc" || initDoc["version"].(float64) != 2 {
		t.Fatalf("init-doc 错误: %v", initDoc)
	}
}

// TestNonLeaderBlockedWhileSyncing 验证快照同步期间非 leader 提交返回
// ErrSyncing（客户端保持 sendable 待 sync-end 补发），leader 提交放行。
func TestNonLeaderBlockedWhileSyncing(t *testing.T) {
	m := NewManager()
	fileID := uuid.New()
	a, _ := m.Join(fileID, uuid.New(), "A", "#1")
	recvN(t, a, 1)
	b, _ := m.Join(fileID, uuid.New(), "B", "#2")
	completeJoin(t, m, a, b, fileID, `{"type":"doc"}`)
	drainPresenceAndSyncEnd(t, a, b)
	// c 待同步：syncing 开始。
	c, _ := m.Join(fileID, uuid.New(), "C", "#3")
	recvN(t, a, 2) // sync-begin + sync-request(c)
	recvN(t, b, 1) // sync-begin
	if err := m.SubmitSteps(b, stepsOf("b", 1), rawClientID(2)); err != ErrSyncing {
		t.Fatalf("非 leader syncing 期间提交错误 = %v, want ErrSyncing", err)
	}
	if err := m.SubmitSteps(a, stepsOf("a", 1), rawClientID(1)); err != nil {
		t.Fatalf("leader syncing 期间提交被拒: %v", err)
	}
	// c 转正后（pending 清空）非 leader 恢复提交。
	if err := m.CompleteSnapshot(a, json.RawMessage(`{"type":"doc"}`), roomVersion(t, m, fileID), c.ConnID); err != nil {
		t.Fatal(err)
	}
	recvN(t, c, 1) // init-doc
	if err := m.SubmitSteps(b, stepsOf("b2", 1), rawClientID(2)); err != nil {
		t.Fatalf("sync-end 后非 leader 提交仍被拒: %v", err)
	}
}

// TestPendingTimeoutFailsJoin 验证 pending 加入者超时未获快照被摘除（send
// 关闭、客户端断开重连），既有成员收到 sync-end 恢复发送。
func TestPendingTimeoutFailsJoin(t *testing.T) {
	old := collabJoinTimeout
	collabJoinTimeout = 50 * time.Millisecond
	t.Cleanup(func() { collabJoinTimeout = old })
	m := NewManager()
	fileID := uuid.New()
	a, _ := m.Join(fileID, uuid.New(), "A", "#1")
	recvN(t, a, 1) // init
	b, _ := m.Join(fileID, uuid.New(), "B", "#2")
	recvN(t, a, 2) // sync-begin + sync-request
	deadline := time.Now().Add(2 * time.Second)
	closed := false
	for time.Now().Before(deadline) {
		if _, ok := <-b.Send(); !ok {
			closed = true
			break
		}
		t.Fatal("pending 期间 joiner 不应收到消息")
	}
	if !closed {
		t.Fatal("超时后 pending joiner 的 send 未关闭")
	}
	end := decode(t, recvN(t, a, 1)[0])
	if end["type"] != "sync-end" {
		t.Fatalf("超时摘除后应广播 sync-end, got %v", end["type"])
	}
}

// TestLeaderLeaveDuringPendingResendsRequest 验证 leader 在快照同步途中离开：
// 转移后向新 leader 重发 sync-request，新 leader 完成转正。
func TestLeaderLeaveDuringPendingResendsRequest(t *testing.T) {
	m := NewManager()
	fileID := uuid.New()
	a, _ := m.Join(fileID, uuid.New(), "A", "#1")
	recvN(t, a, 1)
	b, _ := m.Join(fileID, uuid.New(), "B", "#2")
	completeJoin(t, m, a, b, fileID, `{"type":"doc"}`)
	drainPresenceAndSyncEnd(t, a, b)
	c, _ := m.Join(fileID, uuid.New(), "C", "#3")
	recvN(t, a, 2) // sync-begin + sync-request(c)
	recvN(t, b, 1) // sync-begin
	m.Leave(a)
	// b：leave(a) + presence(b leader) + 重发的 sync-request(c)。
	msgs := recvN(t, b, 3)
	leave, promoted, req := decode(t, msgs[0]), decode(t, msgs[1]), decode(t, msgs[2])
	if leave["type"] != "leave" || promoted["type"] != "presence" || promoted["leader"] != true {
		t.Fatalf("leave/晋升通告错误: %v", msgs)
	}
	if req["type"] != "sync-request" || req["target"] != c.ConnID.String() {
		t.Fatalf("应向新 leader 重发 sync-request: %v", req)
	}
	if err := m.CompleteSnapshot(b, json.RawMessage(`{"type":"doc"}`), roomVersion(t, m, fileID), c.ConnID); err != nil {
		t.Fatal(err)
	}
	if msg := decode(t, recvN(t, c, 1)[0]); msg["type"] != "init-doc" {
		t.Fatalf("c 应被新 leader 转正: %v", msg["type"])
	}
}
