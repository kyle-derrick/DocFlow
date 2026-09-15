// Command wscheck 是 Redis 跨实例 WS 广播的一次性验证程序（验证后删除）：
// 登录 admin → WS 连接实例 A(18081) → 经实例 B(18082) API 创建分享
// （share.create 通知 owner）→ 断言 A 的连接在超时内收到推送。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func fatalf(format string, args ...any) { fmt.Printf("FAIL "+format+"\n", args...); os.Exit(1) }

func main() {
	loginBody := `{"email":"admin@example.com","password":"AdminPassword123"}`
	resp, err := http.Post("http://127.0.0.1:18082/api/v1/auth/login", "application/json", bytes.NewBufferString(loginBody))
	if err != nil {
		fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AccessToken == "" {
		fatalf("login decode (status %d): %v", resp.StatusCode, err)
	}
	fmt.Println("OK login via B (18082)")

	// 连实例 A：token 经 Sec-WebSocket-Protocol 子协议（bearer, <token>）。
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{"bearer", out.AccessToken},
	}
	conn, _, err := dialer.Dial("ws://127.0.0.1:18081/api/v1/ws/notifications", nil)
	if err != nil {
		fatalf("ws dial A: %v", err)
	}
	defer conn.Close()
	fmt.Println("OK ws connected to A (18081)")

	msgs := make(chan string, 4)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				close(msgs)
				return
			}
			msgs <- string(data)
		}
	}()

	// 经实例 B 触发通知事件：创建公开分享（share.created 通知 owner）。
	// file_id 传零值 UUID 会 400——先建一个最小会话拿文件？直接复用分享
	// 创建的 400 不发通知，改用「站内信」最短路径：邀请一个新邮箱
	// （invitation 通知仅收件人），仍不达 admin。这里用 admin 自分享：
	// 需要真实文件 id——经 B 上传一个微型文件（走 /uploads 简化版）。
	fileID := uploadTiny(out.AccessToken)
	shareBody := fmt.Sprintf(`{"file_id":%q,"permission":"download","expires_in":3600}`, fileID)
	req, _ := http.NewRequest("POST", "http://127.0.0.1:18082/api/v1/shares", bytes.NewBufferString(shareBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+out.AccessToken)
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatalf("share create via B: %v", err)
	}
	sresp.Body.Close()
	fmt.Printf("OK share create via B (status %d), waiting for WS push on A...\n", sresp.StatusCode)

	deadline := time.After(15 * time.Second)
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				fatalf("ws read loop closed")
			}
			fmt.Printf("PASS cross-instance ws push: %.160s\n", m)
			return
		case <-deadline:
			fatalf("timeout: no ws push reached instance A")
		}
	}
}

// uploadTiny 经实例 B 完成一次微型上传并返回文件 ID。
func uploadTiny(token string) string {
	payload := `{"name":"wscheck.txt","size":7}`
	req, _ := http.NewRequest("POST", "http://127.0.0.1:18082/api/v1/uploads", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fatalf("upload session: %v", err)
	}
	var sess struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()
	if sess.ID == "" {
		fatalf("upload session id empty (status %d)", resp.StatusCode)
	}

	req, _ = http.NewRequest("PATCH", "http://127.0.0.1:18082/api/v1/uploads/"+sess.ID, bytes.NewBufferString("wscheck"))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fatalf("upload patch: %v", err)
	}
	resp.Body.Close()

	req, _ = http.NewRequest("POST", "http://127.0.0.1:18082/api/v1/uploads/"+sess.ID+"/complete", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fatalf("upload complete: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	var done struct {
		FileID string `json:"file_id"`
		Status string `json:"status"`
	}
	json.Unmarshal(raw, &done)
	if done.FileID == "" {
		fatalf("complete file_id empty (status %d, body %s)", resp.StatusCode, string(raw))
	}
	fmt.Printf("OK upload via B -> %s (%s)\n", done.FileID, done.Status)
	return done.FileID
}
