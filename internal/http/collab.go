package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/collab"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	// collabMaxMessageBytes 单条客户端消息（含 steps 批次）上限 1MB：
	// 超限由 gorilla ReadLimit 强制并直接断开连接（防内存滥用）。
	collabMaxMessageBytes = 1 << 20
	// collabWriteWait 单次写超时；collabPongWait 读超时（任意入站消息
	// ——含应用层 ping——均续期）；collabPingPeriod 服务端写泵协议层
	// ping 周期（30s，须短于 90s 读超时以保活）。
	collabWriteWait  = 10 * time.Second
	collabReadWait   = 90 * time.Second
	collabPingPeriod = 30 * time.Second
)

// collabFileAuthorizer 为加入协作房间前的写权限判定最小依赖（*files.Store
// 满足）：ValidateReplaceTarget 即「覆盖为新版本」链路（WebDAV 写入 /
// ONLYOFFICE 保存回调同源）的写权限入口——文件须存在且未删除、type=file，
// 个人文件要求 owner、团队文件要求 CanWrite（先经路径级 ACL write 判定），
// 不新造权限逻辑、不绕过 ACL。
type collabFileAuthorizer interface {
	ValidateReplaceTarget(user, target uuid.UUID) (files.File, error)
}

var _ collabFileAuthorizer = (*files.Store)(nil)

// SetCollabManager 注入协作房间管理器（幂等）；写权限判定源取 NewHandler
// 装配的 *files.Store。未注入时 GET /api/v1/collab/:fileId/ws 返回 503
// （生产恒注入）。
func (h *Handler) SetCollabManager(m *collab.Manager) {
	if m != nil {
		h.collab = m
		h.collabFiles = h.files
	}
}

// collabEnabled 热读取 collab.enabled（EffectImmediate，默认 true）：
// settings 未注入或读取失败时同样回退启用（与管理端缺省一致）。
func (h *Handler) collabEnabled() bool {
	if h.settings == nil {
		return true
	}
	v, err := h.settings.GetBool(settings.KeyCollabEnabled)
	if err != nil {
		return true
	}
	return v
}

// wsCollab GET /api/v1/collab/:fileId/ws：单文件富文本协作房间（ProseMirror
// prosemirror-collab 的权威排序端点）。认证/Origin/升级与 /ws/notifications
// 完全一致（Sec-WebSocket-Protocol「bearer, <JWT>」子协议 + JWT + origin
// Allowed）；升级前先校验目标文件存在且当前用户具备编辑（写）权限——复用
// files 的 ValidateReplaceTarget 写权限链，无权限 403、文件不存在 404；
// collab.enabled=false 时 404 且不创建房间。
func (h *Handler) wsCollab(c *gin.Context) {
	if h.collab == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "collab service not configured"})
		return
	}
	if !h.collabEnabled() {
		c.Status(http.StatusNotFound)
		return
	}
	if !h.originAllowed(c.GetHeader("Origin")) {
		c.Status(http.StatusForbidden)
		return
	}
	uid, ok := h.wsAuthenticate(c)
	if !ok {
		return
	}
	fileID, ok := parseID(c, c.Param("fileId"))
	if !ok {
		return
	}
	if h.collabFiles == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "collab service not configured"})
		return
	}
	if _, err := h.collabFiles.ValidateReplaceTarget(uid, fileID); err != nil {
		switch {
		case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrInvalidTarget):
			// 不存在/已删除/非文件（如目录）：404，不泄露越权差异。
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		case errors.Is(err, files.ErrForbidden):
			c.JSON(http.StatusForbidden, gin.H{"error": "no permission to edit this file"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to verify file access"})
		}
		return
	}
	upgrader := h.wsUpgrader()
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	h.serveCollabConn(conn, fileID, uid)
}

// serveCollabConn 为已升级的协作连接运行读写循环。协议（JSON 信封，见
// internal/collab）：客户端 join/steps/presence/ping；服务器 init/steps/
// presence/leave/error。满员、被摘除等连接级失败直接断开（客户端整篇重拉）。
func (h *Handler) serveCollabConn(conn *websocket.Conn, fileID, uid uuid.UUID) {
	conn.SetReadLimit(collabMaxMessageBytes)
	_ = conn.SetReadDeadline(time.Now().Add(collabReadWait))
	var (
		participant *collab.Participant
		// out 承载服务端主动下行（error 通告）：写泵启动前直写连接（唯一
		// 写者），启动后经 out 交给写泵（gorilla 连接不允许并发写）。
		out chan []byte
	)
	defer func() {
		if participant != nil {
			h.collab.Leave(participant)
		}
	}()
	writeControl := func(payload []byte) {
		if out == nil {
			_ = conn.SetWriteDeadline(time.Now().Add(collabWriteWait))
			_ = conn.WriteMessage(websocket.TextMessage, payload)
			return
		}
		select {
		case out <- payload:
		default:
		}
	}
	sendError := func(message string) {
		payload, _ := json.Marshal(collab.ErrorMessage{Type: "error", Message: message})
		writeControl(payload)
	}
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			// 读失败（断开/读超时/超限）：直接关闭，由 defer Leave 清理。
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(collabReadWait))
		var msg collab.ClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			sendError("invalid json message")
			continue
		}
		switch msg.Type {
		case "join":
			if participant != nil {
				sendError("already joined")
				continue
			}
			name, color := sanitizeCollabIdentity(msg.Name, msg.Color)
			p, err := h.collab.Join(fileID, uid, name, color)
			if err != nil {
				if errors.Is(err, collab.ErrRoomFull) {
					sendError("collab room is full")
					return
				}
				sendError("unable to join room")
				continue
			}
			participant = p
			out = make(chan []byte, 8)
			// init 快照已作为 p 通道首条消息入队，此处仅启动唯一写泵。
			go h.collabWritePump(conn, p.Send(), out)
		case "steps":
			if participant == nil {
				sendError("not joined")
				continue
			}
			if len(msg.Steps) == 0 {
				sendError("empty steps")
				continue
			}
			if len(msg.ClientID) == 0 {
				sendError("missing clientID")
				continue
			}
			if err := h.collab.SubmitSteps(participant, msg.Steps, msg.ClientID); err != nil {
				switch {
				case errors.Is(err, collab.ErrNotInRoom):
					// 已被房间摘除（慢消费者）：版本序列已失序，断开重连。
					sendError("removed from room")
					return
				case errors.Is(err, collab.ErrSyncing):
					// 快照同步中：客户端保持 sendable 待 sync-end 补发，非致命。
					sendError("syncing")
				default:
					sendError("invalid steps message")
				}
			}
		case "snapshot":
			// leader 快照上报（sync-request 的应答）：转正对应 pending 加入者。
			if participant == nil {
				sendError("not joined")
				continue
			}
			if len(msg.Doc) == 0 || msg.Target == uuid.Nil {
				sendError("invalid snapshot message")
				continue
			}
			if err := h.collab.CompleteSnapshot(participant, msg.Doc, msg.Version, msg.Target); err != nil {
				switch {
				case errors.Is(err, collab.ErrStaleSnapshot):
					// 快照落后于权威版本：服务器已重发 sync-request，非致命。
					sendError("stale snapshot")
				case errors.Is(err, collab.ErrNotLeader):
					sendError("not the leader")
				default:
					// target 已不在 pending（超时摘除/已转正）：忽略。
					sendError("snapshot target gone")
				}
			}
		case "presence":
			if participant == nil {
				sendError("not joined")
				continue
			}
			_ = h.collab.UpdatePresence(participant, msg.Selection)
		case "ping":
			// 应用层保活：读超时已随本消息续期，无需应答。
		default:
			sendError("unknown message type")
		}
	}
}

// collabWritePump 为协作连接的唯一写者：转发房间广播（room）与服务端主动
// 下行（out），并以 30s 周期发协议层 ping（配合 90s 读超时保活）。房间
// 摘除（room 关闭）或任一写失败即关闭连接退出——读循环随之解除阻塞并
// 触发 Leave 清理。
func (h *Handler) collabWritePump(conn *websocket.Conn, room, out <-chan []byte) {
	ticker := time.NewTicker(collabPingPeriod)
	defer ticker.Stop()
	defer conn.Close()
	write := func(messageType int, payload []byte) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(collabWriteWait))
		return conn.WriteMessage(messageType, payload) == nil
	}
	for {
		select {
		case msg, ok := <-room:
			if !ok || !write(websocket.TextMessage, msg) {
				return
			}
		case msg := <-out:
			if !write(websocket.TextMessage, msg) {
				return
			}
		case <-ticker.C:
			if !write(websocket.PingMessage, nil) {
				return
			}
		}
	}
}

// sanitizeCollabIdentity 归一化客户端 join 携带的展示名与光标颜色：去
// 首尾空白、缺省回退、截断超长（防滥用；均为纯展示字段，不做格式强校验）。
func sanitizeCollabIdentity(name, color string) (string, string) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "匿名协作者"
	}
	if runes := []rune(name); len(runes) > 64 {
		name = string(runes[:64])
	}
	color = strings.TrimSpace(color)
	if color == "" {
		color = "#6b7280"
	}
	if runes := []rune(color); len(runes) > 32 {
		color = string(runes[:32])
	}
	return name, color
}
