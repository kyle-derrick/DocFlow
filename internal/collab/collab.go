// Package collab 提供富文本实时协作的服务端房间管理（ProseMirror
// prosemirror-collab 客户端的权威排序端点，HTTP 侧见 internal/http/collab.go）。
//
// 服务器不理解 ProseMirror steps 的内容语义，只做四件事：
//   - JSON 透传（steps/selection 原样转发，保留 clientID 的原始 JSON 形态）；
//   - 统一排序：房间内所有消息按到达顺序在房间锁下串行化，保证每个参与者
//     看到完全一致的消息序列；
//   - 广播给房间全部参与者（含发送者——客户端按 clientID 忽略自己的 steps）；
//   - 版本计数：版本为房间内单调递增整数，仅累计收到的 steps 总数。
//   - 后加入者一致性：磁盘文档可能落后于房间内尚未保存的协作 steps，
//     非空房的新加入者不直接以磁盘文档为基，而是经「sync-begin 暂停 →
//     leader 冲账后上报实时快照 → 服务器校验版本原子转正（init-doc）→
//     sync-end 恢复」流程拿到一致的起点文档。
//
// 已知局限（有意为之，见任务边界）：
//   - 房间为每实例内存态，多实例部署时同一文件的协作者连到不同实例会落入
//     不同房间、版本序列互相独立（不引入 Redis 跨实例桥；单实例或按文件
//     粘性会话的部署下语义完整）；
//   - 不保存历史 steps：断线客户端重连即整篇重拉（init 只带当前版本号与
//     参与者名单），不做断线 steps 追赶/补发；
//   - 空房间在最后一名参与者离开时即从 Manager 摘除（内存回收）。
package collab

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultMaxParticipants 为单文件协作房间的缺省参与者上限（满员后加入被
// 拒绝并返回 ErrRoomFull）。可经 SetMaxParticipants 覆盖（装配期调用）。
const DefaultMaxParticipants = 20

// sendBuffer 为单个参与者发送缓冲深度：写泵消费过慢（如网络阻塞）导致
// 缓冲写满时，该参与者被视为失效连接摘除——协作 steps 一旦丢序即错版，
// 宁可断开让对方整篇重拉，也不允许排队积压后乱序送达。
const sendBuffer = 64

var (
	// ErrRoomFull 表示协作房间已达参与者上限。
	ErrRoomFull = errors.New("collab room is full")
	// ErrNotInRoom 表示参与者已不在房间（被摘除或房间已清理），其后续
	// 提交（steps/presence）不再进入版本序列。
	ErrNotInRoom = errors.New("collab participant is no longer in the room")
	// ErrSyncing 表示房间正在快照同步（有 pending 加入者）：非 leader 的
	// steps 被暂缓（客户端应保持 sendable 待 sync-end 后补发，非致命）。
	ErrSyncing = errors.New("collab room is syncing a snapshot")
	// ErrStaleSnapshot 表示 leader 上报的快照版本落后于房间权威版本
	//（上报时仍有在途 steps）：服务器已向 leader 重发 sync-request，等其
	// 追平后重新上报（非致命，客户端忽略即可）。
	ErrStaleSnapshot = errors.New("collab snapshot is stale")
	// ErrNotLeader 表示快照上报者不是当前 leader（不可上报）。
	ErrNotLeader = errors.New("collab snapshot sender is not the leader")
)

// collabJoinTimeout 为 pending 加入者等待 leader 快照的时限：超时即摘除
// 该连接（客户端断开重连；重连时房间若已空则成为首成员直接转正）。包级
// 变量仅供测试缩短时长。
var collabJoinTimeout = 10 * time.Second

// Manager 管理按文件划分的协作房间（fileUUID → Room）。rooms 与房间创建
// 由 mu 保护；房间内部的成员变更、版本递增与广播由各 Room 自身的锁串行化
// （锁序恒为 Manager.mu → Room.mu，反向获取不存在，无死锁）。
type Manager struct {
	mu              sync.Mutex
	rooms           map[uuid.UUID]*Room
	maxParticipants int
}

// NewManager 创建房间管理器（参与者上限为 DefaultMaxParticipants）。
func NewManager() *Manager {
	return &Manager{rooms: make(map[uuid.UUID]*Room), maxParticipants: DefaultMaxParticipants}
}

// SetMaxParticipants 调整单房间参与者上限（n<1 忽略）。仅应在装配期、
// 开始接受连接前调用。
func (m *Manager) SetMaxParticipants(n int) {
	if n >= 1 {
		m.maxParticipants = n
	}
}

// RoomCount 返回当前存活的房间数（空房自动清理后的观察值，测试/运维用）。
func (m *Manager) RoomCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms)
}

// Room 为单个文件的协作房间：participants 按加入顺序排列（index 0 恒为
// leader），version 为房间内单调递增的权威版本号。
type Room struct {
	fileID uuid.UUID
	mu     sync.Mutex
	// participants 按加入顺序排列；最早的在册成员为 leader（离开后转移
	// 给下一个，见 detachLocked 的调用方）。
	participants []*Participant
	// pending 为已通过 Join 但尚未拿到 leader 快照、未进入广播列表的
	// 加入者（后加入者一致性：磁盘文档可能落后于房间内未保存的 steps，
	// 必须以 leader 实时文档为基，见 Join/CompleteSnapshot）。
	pending []*Participant
	version int64
}

// Version 返回房间当前权威版本号（累计收到的 steps 总数）。
func (r *Room) Version() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.version
}

// Participant 为房间内的一名参与者（对应一条 WebSocket 连接）。
// ConnID 为服务器分配的连接标识（每连接唯一，与 ProseMirror 客户端的
// clientID 无关）；展示名与光标颜色来自客户端 join 消息（加入后不可变）。
type Participant struct {
	ConnID uuid.UUID
	UserID uuid.UUID

	name   string
	color  string
	leader bool
	room   *Room
	send   chan []byte
	closed bool // send 已关闭（仅在 Room.mu 下由 detachLocked 变更）
}

// Send 返回房间广播的接收通道：缓冲满即被摘除并关闭（写泵随之退出）。
func (p *Participant) Send() <-chan []byte { return p.send }

// ParticipantInfo 为参与者的对外视图（init/presence 消息载荷字段）。
type ParticipantInfo struct {
	ConnID uuid.UUID `json:"connId"`
	UserID uuid.UUID `json:"userId"`
	Name   string    `json:"name"`
	Color  string    `json:"color"`
	Leader bool      `json:"leader"`
}

// ClientMessage 为客户端→服务器消息信封。服务器只识别 type 并透传负载，
// 不理解 steps/selection 的内容语义。type 取值：
// join（携带 name/color）| steps | presence | ping | snapshot（leader 快照
// 上报，携带 doc/version/target）。
type ClientMessage struct {
	Type string `json:"type"`
	// Name/Color 仅 join 消息携带（展示名与光标颜色）。
	Name  string `json:"name,omitempty"`
	Color string `json:"color,omitempty"`
	// Steps 为 ProseMirror step 的 JSON 数组（原样透传，不解析内容）。
	Steps []json.RawMessage `json:"steps,omitempty"`
	// ClientID 为 steps 发起方的客户端标识（原样透传，保留数值/字符串
	// 形态——prosemirror-collab 的 clientID 通常为整数）。
	ClientID json.RawMessage `json:"clientID,omitempty"`
	// Version 为客户端本地版本号；服务器不消费（以自身权威序列为准），
	// 保留字段以便前端对齐 prosemirror-collab 协议。snapshot 消息中为
	// leader 上报快照所对应的权威版本（须与房间当前版本一致，否则视为
	// 过期快照重传）。
	Version int64 `json:"version,omitempty"`
	// Selection 为选区（光标）信息，通常为 {anchor,head}（原样透传）。
	Selection json.RawMessage `json:"selection,omitempty"`
	// Doc 仅 snapshot 消息携带：leader 序列化的实时文档 JSON（Tiptap
	// getJSON 产物，原样透传给目标 pending 加入者，服务器不解析）。
	Doc json.RawMessage `json:"doc,omitempty"`
	// Target 仅 snapshot 消息携带：本次快照对应的 pending 加入者连接 ID
	//（来自 sync-request.target，原样回填）。
	Target uuid.UUID `json:"target,omitempty"`
}

// initMessage 加入成功后下发给该参与者的房间快照——作为其 send 通道的
// 第一条消息，保证先于任何广播到达（客户端须先拿到版本与名单）。仅当
// 加入时房间为空（本端即首成员/leader，权威版本恒为 0）时使用；非空房
// 走 leader 快照流程，见 initDocMessage。
type initMessage struct {
	Type         string            `json:"type"` // "init"
	Version      int64             `json:"version"`
	Participants []ParticipantInfo `json:"participants"`
}

// syncBeginMessage 通知既有成员暂停 steps 发送（有新成员待同步）：客户端
// 冲账本地 sendable（leader 例外，见 sync-request），非 leader 暂缓提交。
type syncBeginMessage struct {
	Type string `json:"type"` // "sync-begin"
}

// syncRequestMessage 请求 leader 上报实时文档快照：target 为待转正的
// pending 加入者连接 ID（leader 回填到 snapshot.target）。
type syncRequestMessage struct {
	Type   string    `json:"type"` // "sync-request"
	Target uuid.UUID `json:"target"`
}

// syncEndMessage 快照同步完成：既有成员恢复 steps 发送（version 为当前
// 权威版本，客户端可据此校验）。
type syncEndMessage struct {
	Type    string `json:"type"` // "sync-end"
	Version int64  `json:"version"`
}

// initDocMessage 非空房加入者的首条消息（代替 init）：doc 为 leader 上报
// 的实时文档（含未保存的协作 steps），version 为其对应权威版本，客户端
// 以此为基初始化编辑器与 collab 插件版本。
type initDocMessage struct {
	Type         string            `json:"type"` // "init-doc"
	Doc          json.RawMessage   `json:"doc"`
	Version      int64             `json:"version"`
	Participants []ParticipantInfo `json:"participants"`
}

// stepsMessage 服务端权威排序后的 steps 广播：发给房间全部参与者（含
// 发送者，客户端按 clientID 忽略自己的）；version 为本批最后一步落在的
// 权威版本号；clientIDs 与 steps 逐项对应（同批同源，恒为同一 clientID）。
type stepsMessage struct {
	Type      string            `json:"type"` // "steps"
	Version   int64             `json:"version"`
	Steps     []json.RawMessage `json:"steps"`
	ClientIDs []json.RawMessage `json:"clientIDs"`
}

// presenceMessage 参与者上线通告与选区更新；上线通告（新加入/leader 转移）
// 时 selection 为 null。
type presenceMessage struct {
	Type string `json:"type"` // "presence"
	ParticipantInfo
	Selection json.RawMessage `json:"selection"`
}

// leaveMessage 参与者离开（连接断开/被摘除）通告。
type leaveMessage struct {
	Type   string    `json:"type"` // "leave"
	ConnID uuid.UUID `json:"connId"`
}

// ErrorMessage 为服务器→客户端的错误通告（未知消息类型、非法载荷等；
// 连接级错误如超限/超时直接断开，不经此信封）。
type ErrorMessage struct {
	Type    string `json:"type"` // "error"
	Message string `json:"message"`
}

// Join 把 (userID, name, color) 加入 fileID 的协作房间（无房则创建），
// 返回新参与者。两种路径：
//   - 空房：本端即首成员（leader），立即转正并下发 init（权威版本 0）；
//   - 非空房：暂缓入列（pending，不进入广播），向既有成员广播 sync-begin
//     （暂停 steps），并向 leader 发 sync-request；leader 上报实时快照后
//     经 CompleteSnapshot 原子转正（下发 init-doc + 广播 sync-end）。
//     超时（collabJoinTimeout）未完成则摘除该连接。
//
// 满员返回 ErrRoomFull。
func (m *Manager) Join(fileID, userID uuid.UUID, name, color string) (*Participant, error) {
	m.mu.Lock()
	r := m.rooms[fileID]
	if r == nil {
		r = &Room{fileID: fileID}
		m.rooms[fileID] = r
	}
	// 先取房间锁再放管理器锁：期间该房间不可能被空房清理从 map 摘除
	// （清理路径须先持 Manager.mu 再复核 Room.mu，见 Leave），避免
	// 「Join 拿到即将被删的房实例」造成同文件房间分裂。
	r.mu.Lock()
	max := m.maxParticipants
	m.mu.Unlock()
	defer r.mu.Unlock()
	if len(r.participants)+len(r.pending) >= max {
		return nil, ErrRoomFull
	}
	p := &Participant{ConnID: uuid.New(), UserID: userID, name: name, color: color, room: r, send: make(chan []byte, sendBuffer)}
	if len(r.participants) == 0 && len(r.pending) == 0 {
		// 空房首成员：立即转正（首成员恒为 leader）。
		p.leader = true
		r.participants = append(r.participants, p)
		r.deliverLocked(p, marshalMessage(initMessage{Type: "init", Version: r.version, Participants: r.snapshotLocked()}))
		return p, nil
	}
	// 非空房：pending 加入，等 leader 快照（后加入者一致性）。
	r.pending = append(r.pending, p)
	r.broadcastLocked(marshalMessage(syncBeginMessage{Type: "sync-begin"}), nil)
	r.deliverLocked(r.participants[0], marshalMessage(syncRequestMessage{Type: "sync-request", Target: p.ConnID}))
	go m.failPendingAfter(r, p.ConnID, collabJoinTimeout)
	return p, nil
}

// failPendingAfter 在超时后摘除仍未转正的 pending 加入者（关闭其 send 即
// 断开连接；客户端重连）。若 pending 因此清空，向既有成员广播 sync-end
// 恢复 steps 发送。
func (m *Manager) failPendingAfter(r *Room, connID uuid.UUID, timeout time.Duration) {
	time.AfterFunc(timeout, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		dropped := r.dropPendingLocked(connID)
		if dropped && len(r.pending) == 0 && len(r.participants) > 0 {
			r.broadcastLocked(marshalMessage(syncEndMessage{Type: "sync-end", Version: r.version}), nil)
		}
	})
}

// SubmitSteps 把一个客户端提交的 steps 批次串行化进房间权威序列：版本按
// 批内 steps 数递增（仅计数），随后把整个批次作为一条消息广播给房间全部
// 参与者（含发送者）。并发提交在房间锁下天然按到达顺序串行化——同一批次
// 的 steps 绝不与其他批次交错。参与者已被摘除时返回 ErrNotInRoom。
// 快照同步期间（存在 pending）仅 leader 可提交：非 leader 返回 ErrSyncing
// （客户端保持 sendable 待 sync-end 补发，非致命）——leader 的在途冲账
// steps 被放行，其上报快照的版本校验（CompleteSnapshot）由此收敛。
func (m *Manager) SubmitSteps(p *Participant, steps []json.RawMessage, clientID json.RawMessage) error {
	r := p.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.containsLocked(p) {
		return ErrNotInRoom
	}
	if len(steps) == 0 {
		return nil
	}
	if len(r.pending) > 0 && !p.leader {
		return ErrSyncing
	}
	r.version += int64(len(steps))
	ids := make([]json.RawMessage, len(steps))
	for i := range ids {
		ids[i] = clientID
	}
	r.broadcastLocked(marshalMessage(stepsMessage{Type: "steps", Version: r.version, Steps: steps, ClientIDs: ids}), nil)
	return nil
}

// UpdatePresence 向房间广播该参与者的选区（光标）更新（selection 原样
// 透传）。参与者已被摘除时返回 ErrNotInRoom。
func (m *Manager) UpdatePresence(p *Participant, selection json.RawMessage) error {
	r := p.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.containsLocked(p) {
		return ErrNotInRoom
	}
	r.broadcastLocked(marshalMessage(presenceMessage{Type: "presence", ParticipantInfo: p.info(), Selection: selection}), nil)
	return nil
}

// CompleteSnapshot 处理 leader 上报的实时文档快照，把对应 pending 加入者
// 原子转正（同一次房间锁持有内完成，杜绝快照与广播交错）：
//   - 校验上报者是当前 leader、target 仍为 pending、版本与房间权威版本
//     一致（不一致 = 快照过期：向 leader 重发 sync-request 待其追平重报，
//     返回 ErrStaleSnapshot，非致命）；
//   - 转正：追加进 participants，向其下发 init-doc（doc/version/全量
//     名单），向既有成员广播其 presence 上线；
//   - pending 清空后广播 sync-end，既有成员恢复 steps 发送。
func (m *Manager) CompleteSnapshot(leader *Participant, doc json.RawMessage, version int64, target uuid.UUID) error {
	r := leader.room
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.containsLocked(leader) || !leader.leader {
		return ErrNotLeader
	}
	idx := -1
	for i, pj := range r.pending {
		if pj.ConnID == target {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ErrNotInRoom
	}
	if version != r.version || len(doc) == 0 {
		r.deliverLocked(leader, marshalMessage(syncRequestMessage{Type: "sync-request", Target: target}))
		return ErrStaleSnapshot
	}
	p := r.pending[idx]
	r.pending = append(r.pending[:idx], r.pending[idx+1:]...)
	r.participants = append(r.participants, p)
	r.deliverLocked(p, marshalMessage(initDocMessage{Type: "init-doc", Doc: doc, Version: version, Participants: r.snapshotLocked()}))
	// 既有成员此前未见过该 pending 成员：转正后补发上线通告。
	r.broadcastLocked(marshalMessage(presenceMessage{Type: "presence", ParticipantInfo: p.info()}), p)
	if len(r.pending) == 0 {
		r.broadcastLocked(marshalMessage(syncEndMessage{Type: "sync-end", Version: r.version}), p)
	}
	return nil
}

// Leave 摘除参与者并向房间广播 leave；leader 离开时转交给最早加入的剩余
// 参与者并广播其 presence（leader=true）。pending（未转正）参与者离开仅
// 从待同步队列摘除并关闭其 send；leader 离开且仍有 pending 时向新 leader
// 重发 sync-request。房间清空（且无 pending）后从 Manager 摘除（空房自动
// 清理）；成员清空但仍有 pending（无 leader 可供快照）时一并摘除 pending
// 并清理房间。幂等：已不在房间时 no-op。
func (m *Manager) Leave(p *Participant) {
	if p == nil || p.room == nil {
		return
	}
	r := p.room
	r.mu.Lock()
	if !r.containsLocked(p) {
		// 未知在册成员：可能是未转正的 pending 参与者。
		if r.dropPendingLocked(p.ConnID) {
			if len(r.pending) == 0 && len(r.participants) > 0 {
				r.broadcastLocked(marshalMessage(syncEndMessage{Type: "sync-end", Version: r.version}), nil)
			}
		}
		empty := len(r.participants) == 0 && len(r.pending) == 0
		r.mu.Unlock()
		if empty {
			m.cleanupRoom(r)
		}
		return
	}
	wasLeader := p.leader
	r.broadcastLocked(marshalMessage(leaveMessage{Type: "leave", ConnID: p.ConnID}), p)
	// 广播（含摘除慢消费者）完成后才移除自己，保证 leave 通告里不包含
	// 离开者自身收到的最后一批消息语义混乱。
	r.detachLocked(p)
	if wasLeader && len(r.participants) > 0 {
		next := r.participants[0]
		next.leader = true
		r.broadcastLocked(marshalMessage(presenceMessage{Type: "presence", ParticipantInfo: next.info()}), nil)
		// leader 中途离开但仍有待同步加入者：向新 leader 重发快照请求
		//（新 leader 此前已收到 sync-begin，直接补发请求即可）。
		for _, pj := range r.pending {
			r.deliverLocked(next, marshalMessage(syncRequestMessage{Type: "sync-request", Target: pj.ConnID}))
		}
	}
	if len(r.participants) == 0 {
		// 成员清空：剩余 pending 无 leader 可供快照，一并摘除（其连接
		// 随 send 关闭断开，客户端重连即成为新房首成员）。
		for _, pj := range r.pending {
			if !pj.closed {
				pj.closed = true
				close(pj.send)
			}
		}
		r.pending = nil
	}
	empty := len(r.participants) == 0 && len(r.pending) == 0
	r.mu.Unlock()
	if empty {
		m.cleanupRoom(r)
	}
}

// cleanupRoom 空房清理：锁序 Manager.mu → Room.mu（复核房间仍注册且仍为
// 空——期间可能已有新 Join 入住，此时不删）。
func (m *Manager) cleanupRoom(r *Room) {
	m.mu.Lock()
	if cur, ok := m.rooms[r.fileID]; ok && cur == r {
		r.mu.Lock()
		still := len(r.participants) == 0 && len(r.pending) == 0
		r.mu.Unlock()
		if still {
			delete(m.rooms, r.fileID)
		}
	}
	m.mu.Unlock()
}

// dropPendingLocked 从待同步队列摘除指定 pending 参与者并关闭其 send
// （幂等）；未找到返回 false。
func (r *Room) dropPendingLocked(connID uuid.UUID) bool {
	for i, pj := range r.pending {
		if pj.ConnID != connID {
			continue
		}
		r.pending = append(r.pending[:i], r.pending[i+1:]...)
		if !pj.closed {
			pj.closed = true
			close(pj.send)
		}
		return true
	}
	return false
}

// broadcastLocked 把 msg 非阻塞投递给房间全部参与者（except 除外）。
// 发送缓冲写满的参与者视为失效连接：从房间摘除并关闭其 send（写泵随之
// 退出、连接断开），再向剩余参与者补发该参与者的 leave 通告；若被摘除者
// 是 leader，同时转移并通告新 leader。迭代处理直至不再产生新的摘除。
func (r *Room) broadcastLocked(msg []byte, except *Participant) {
	if msg == nil {
		return
	}
	pending := r.tryDeliverLocked(msg, except)
	for len(pending) > 0 {
		dead := pending
		pending = nil
		for _, p := range dead {
			wasLeader := p.leader
			r.detachLocked(p)
			pending = append(pending, r.tryDeliverLocked(marshalMessage(leaveMessage{Type: "leave", ConnID: p.ConnID}), nil)...)
			if wasLeader && len(r.participants) > 0 {
				next := r.participants[0]
				next.leader = true
				pending = append(pending, r.tryDeliverLocked(marshalMessage(presenceMessage{Type: "presence", ParticipantInfo: next.info()}), nil)...)
			}
		}
	}
}

// tryDeliverLocked 非阻塞投递 msg，返回投递失败（缓冲满）的参与者。
func (r *Room) tryDeliverLocked(msg []byte, except *Participant) []*Participant {
	var failed []*Participant
	for _, p := range r.participants {
		if p == except {
			continue
		}
		select {
		case p.send <- msg:
		default:
			failed = append(failed, p)
		}
	}
	return failed
}

// deliverLocked 向指定参与者投递消息（点对点：init/init-doc/sync-request
// 等）。非阻塞：接收方缓冲满（慢消费者）时按失效处理——摘除并广播 leave、
// 转移 leader（与 broadcastLocked 同策），绝不持锁阻塞（sync-request 可能
// 投给既有 leader，阻塞会卡死整个房间）。Join/CompleteSnapshot 的新通道
// 必空，正常路径不受影响。
func (r *Room) deliverLocked(p *Participant, msg []byte) {
	if msg == nil || p.closed {
		return
	}
	select {
	case p.send <- msg:
	default:
		wasLeader := p.leader
		r.detachLocked(p)
		r.broadcastLocked(marshalMessage(leaveMessage{Type: "leave", ConnID: p.ConnID}), nil)
		if wasLeader && len(r.participants) > 0 {
			next := r.participants[0]
			next.leader = true
			r.broadcastLocked(marshalMessage(presenceMessage{Type: "presence", ParticipantInfo: next.info()}), nil)
		}
	}
}

// detachLocked 从房间摘除参与者并关闭其 send（幂等：重复调用 no-op）。
func (r *Room) detachLocked(p *Participant) {
	for i, cur := range r.participants {
		if cur != p {
			continue
		}
		r.participants = append(r.participants[:i], r.participants[i+1:]...)
		break
	}
	if !p.closed {
		p.closed = true
		close(p.send)
	}
}

func (r *Room) containsLocked(p *Participant) bool {
	for _, cur := range r.participants {
		if cur == p {
			return true
		}
	}
	return false
}

func (r *Room) snapshotLocked() []ParticipantInfo {
	out := make([]ParticipantInfo, 0, len(r.participants))
	for _, p := range r.participants {
		out = append(out, p.info())
	}
	return out
}

func (p *Participant) info() ParticipantInfo {
	return ParticipantInfo{ConnID: p.ConnID, UserID: p.UserID, Name: p.name, Color: p.color, Leader: p.leader}
}

// marshalMessage 序列化下行消息；失败返回 nil（调用方跳过该条广播——
// 本包消息体均为受控结构 + 已通过 JSON 解析的 RawMessage，实际不会失败）。
func marshalMessage(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}
