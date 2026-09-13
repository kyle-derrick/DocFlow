package upload

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClamAVWriteChunkEncoding(t *testing.T) {
	var buf bytes.Buffer
	if err := clamavWriteChunk(&buf, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if err := clamavWriteChunk(&buf, nil); err != nil {
		t.Fatal(err)
	}
	want := append([]byte{0, 0, 0, 4}, []byte("abcd")...)
	want = append(want, 0, 0, 0, 0)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("frames = %v, want %v", buf.Bytes(), want)
	}
}

func TestClamAVChunkingAcrossBoundary(t *testing.T) {
	content := bytes.Repeat([]byte("x"), clamavChunkSize*2+5)
	var buf bytes.Buffer
	if err := clamavWriteChunks(&buf, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	// 解析帧序列：应为 3 个数据块（1024/1024/5）+ 终止 0 长度块。
	var got bytes.Buffer
	head := make([]byte, 4)
	sizes := []int{}
	for {
		if _, err := io.ReadFull(&buf, head); err != nil {
			t.Fatalf("read frame head: %v", err)
		}
		n := binary.BigEndian.Uint32(head)
		if n == 0 {
			break
		}
		sizes = append(sizes, int(n))
		if _, err := io.CopyN(&got, &buf, int64(n)); err != nil {
			t.Fatalf("read frame body: %v", err)
		}
	}
	if !slices.Equal(sizes, []int{clamavChunkSize, clamavChunkSize, 5}) {
		t.Fatalf("frame sizes = %v", sizes)
	}
	if !bytes.Equal(got.Bytes(), content) {
		t.Fatal("reassembled content mismatch")
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected trailing bytes: %v", buf.Bytes())
	}
}

func TestParseClamAVResponse(t *testing.T) {
	cases := []struct {
		resp    string
		wantErr string // 空 表示期望 nil
	}{
		{"OK\x00", ""},
		{"stream: OK\x00", ""},
		{"stream: Eicar-Test-Signature FOUND\x00", "Eicar-Test-Signature"},
		{"Eicar-Test-Signature FOUND\x00", "Eicar-Test-Signature"},
		{"stream: Win.Trojan.Example-1 FOUND\x00", "Win.Trojan.Example-1"},
		{"INSTREAM size limit exceeded. ERROR\x00", "unexpected response"},
		{"\x00", "empty response"},
		{"", "empty response"},
	}
	for _, c := range cases {
		err := parseClamAVResponse([]byte(c.resp))
		if c.wantErr == "" {
			if err != nil {
				t.Fatalf("parse(%q) = %v, want nil", c.resp, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("parse(%q) = %v, want contains %q", c.resp, err, c.wantErr)
		}
	}
}

// pipeClamd 用 net.Pipe 模拟 clamd：完整解析 INSTREAM 会话，
// 把收到的内容交给 respond 决定应答（返回空串表示不回应答直接断连），
// 收到的原始内容通过 channel 传回供断言。
func pipeClamd(t *testing.T, respond func(content []byte) string) (net.Conn, <-chan []byte) {
	t.Helper()
	client, server := net.Pipe()
	contentCh := make(chan []byte, 1)
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	go func() {
		defer server.Close()
		cmd := make([]byte, len(clamavCommand))
		if _, err := io.ReadFull(server, cmd); err != nil || !bytes.Equal(cmd, clamavCommand) {
			contentCh <- nil
			return
		}
		var content bytes.Buffer
		head := make([]byte, 4)
		for {
			if _, err := io.ReadFull(server, head); err != nil {
				contentCh <- nil
				return
			}
			n := binary.BigEndian.Uint32(head)
			if n == 0 {
				break
			}
			if _, err := io.CopyN(&content, server, int64(n)); err != nil {
				contentCh <- nil
				return
			}
		}
		contentCh <- content.Bytes()
		if resp := respond(content.Bytes()); resp != "" {
			server.Write([]byte(resp))
		}
	}()
	return client, contentCh
}

func newPipeScanner(conn net.Conn, timeout time.Duration, required bool) *ClamAVScanner {
	s := NewClamAVScanner("pipe", timeout, required)
	s.dial = func(string, time.Duration) (net.Conn, error) { return conn, nil }
	return s
}

func TestClamAVScanClean(t *testing.T) {
	conn, got := pipeClamd(t, func([]byte) string { return "stream: OK\x00" })
	s := newPipeScanner(conn, time.Minute, true)
	if err := s.Scan(strings.NewReader("clean content")); err != nil {
		t.Fatalf("scan clean: %v", err)
	}
	if content := <-got; string(content) != "clean content" {
		t.Fatalf("clamd received %q", content)
	}
}

func TestClamAVScanFound(t *testing.T) {
	conn, got := pipeClamd(t, func([]byte) string { return "stream: Eicar-Test-Signature FOUND\x00" })
	s := newPipeScanner(conn, time.Minute, true)
	err := s.Scan(strings.NewReader("bad content"))
	if err == nil || !strings.Contains(err.Error(), "Eicar-Test-Signature") {
		t.Fatalf("expected malware error, got %v", err)
	}
	if content := <-got; string(content) != "bad content" {
		t.Fatalf("clamd received %q", content)
	}
}

// 断连（clamd 未回应答即关闭）必须 fail closed，required=false 也不例外。
func TestClamAVScanDisconnectFailsClosed(t *testing.T) {
	for _, required := range []bool{true, false} {
		conn, got := pipeClamd(t, func([]byte) string { return "" })
		s := newPipeScanner(conn, time.Minute, required)
		if err := s.Scan(strings.NewReader("x")); err == nil {
			t.Fatalf("required=%v: expected error on abrupt disconnect", required)
		}
		<-got
	}
}

// clamd 卡死不读数据时，超时 deadline 生效并返回错误（fail closed）。
func TestClamAVScanTimeoutFailsClosed(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	// 服务端只占用连接不读不写，客户端写命令即阻塞至 deadline。
	go func() { io.Copy(io.Discard, server) }()
	s := newPipeScanner(client, 200*time.Millisecond, true)
	start := time.Now()
	if err := s.Scan(strings.NewReader("x")); err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, deadline not enforced", elapsed)
	}
}

// 拨号失败：required=true 必须 fail closed。
func TestClamAVDialFailClosed(t *testing.T) {
	s := NewClamAVScanner("127.0.0.1:1", time.Second, true)
	s.dial = func(string, time.Duration) (net.Conn, error) { return nil, errors.New("dial boom") }
	if err := s.Scan(strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "clamav unreachable") {
		t.Fatalf("expected unreachable error, got %v", err)
	}
}

// 拨号失败：required=false 降级放行并记录警告日志（CLAMAV_REQUIRED=false）。
func TestClamAVDialFailOpenWithWarning(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	s := NewClamAVScanner("127.0.0.1:1", time.Second, false)
	s.dial = func(string, time.Duration) (net.Conn, error) { return nil, errors.New("dial boom") }
	if err := s.Scan(strings.NewReader("x")); err != nil {
		t.Fatalf("required=false should allow, got %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "WARNING") {
		t.Fatalf("expected warning log, got %q", out)
	}
}

func TestSelectScanner(t *testing.T) {
	if _, ok := SelectScanner(false, "localhost:3310", true, 0).(AllowScanner); !ok {
		t.Fatal("scan disabled must select AllowScanner")
	}
	if _, ok := SelectScanner(true, "", true, 0).(RejectScanner); !ok {
		t.Fatal("scan enabled without CLAMAV_ADDR must fail closed with RejectScanner")
	}
	s, ok := SelectScanner(true, "localhost:3310", false, 3*time.Second).(*ClamAVScanner)
	if !ok {
		t.Fatal("scan enabled with CLAMAV_ADDR must select ClamAVScanner")
	}
	if s.addr != "localhost:3310" || s.required || s.timeout != 3*time.Second {
		t.Fatalf("unexpected scanner config: %#v", s)
	}
	// 超时 <= 0 归一为默认 5 分钟。
	if s := SelectScanner(true, "localhost:3310", true, 0).(*ClamAVScanner); s.timeout != clamavDefaultTimeout {
		t.Fatalf("timeout = %v, want default %v", s.timeout, clamavDefaultTimeout)
	}
}
