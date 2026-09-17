package upload

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"
)

// ClamAVScanner 通过 clamd 的 INSTREAM 命令扫描内容（TCP 协议）。
//
// 协议：发送 "zINSTREAM\0"，随后按 <4 字节大端长度 + 数据> 分块发送内容，
// 最后发送 0 长度块终止；clamd 应答以 \0 结尾：
//   - "OK" / "stream: OK"        -> 干净，返回 nil
//   - "stream: <病毒名> FOUND"    -> 拒绝，返回含病毒名的错误
//   - 其他（含断连、超时、空应答） -> 错误
//
// 连接失败默认 fail closed（返回错误，不静默放行）；
// required=false 时拨号失败降级为放行并记录警告日志（CLAMAV_REQUIRED=false）。
type ClamAVScanner struct {
	addr     string
	timeout  time.Duration
	required bool
	dial     func(addr string, timeout time.Duration) (net.Conn, error)
}

const (
	clamavDefaultTimeout = 5 * time.Minute
	// clamavDialTimeoutCap 拨号阶段超时上限：与传输超时解耦——ClamAV
	// 不可达（fail closed 场景）时快速失败，避免同步 complete 请求
	// 挂满整个 CLAMAV_TIMEOUT（默认 5 分钟）导致前端一直"提交处理..."。
	clamavDialTimeoutCap = 5 * time.Second
	// clamavChunkSize 单个 INSTREAM 分块大小，取保守安全值以兼容各版本 clamd。
	clamavChunkSize = 1024
)

// clamavCommand 为 zINSTREAM 命令（z 前缀表示应答以 \0 结尾）。
var clamavCommand = []byte("zINSTREAM\x00")

// ErrMalwareDetected 表示 clamd 应答 FOUND（检出恶意内容）；
// 指标层据此区分 infected 与扫描器错误（error）。
var ErrMalwareDetected = errors.New("clamav: malware detected")

// NewClamAVScanner 创建 ClamAV 扫描器；timeout <= 0 时使用默认 5 分钟。
func NewClamAVScanner(addr string, timeout time.Duration, required bool) *ClamAVScanner {
	if timeout <= 0 {
		timeout = clamavDefaultTimeout
	}
	return &ClamAVScanner{addr: addr, timeout: timeout, required: required, dial: clamavDial}
}

func clamavDial(addr string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.Dial("tcp", addr)
}

func (s *ClamAVScanner) Scan(r io.Reader) error {
	// 拨号用短超时（cap 5s）：不可达时快速失败；建连后的会话仍用完整
	// timeout（大文件 INSTREAM 传输可能确实需要分钟级）。
	dialTimeout := s.timeout
	if dialTimeout > clamavDialTimeoutCap || dialTimeout <= 0 {
		dialTimeout = clamavDialTimeoutCap
	}
	conn, err := s.dial(s.addr, dialTimeout)
	if err != nil {
		if s.required {
			return fmt.Errorf("clamav unreachable at %s: %w", s.addr, err)
		}
		log.Printf("WARNING: ClamAV at %s unreachable (CLAMAV_REQUIRED=false), allowing unscanned content: %v", s.addr, err)
		return nil
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(s.timeout)); err != nil {
		return fmt.Errorf("clamav set deadline: %w", err)
	}
	return clamavScanConn(conn, r)
}

// clamavScanConn 在已建立的连接上完成一次 INSTREAM 会话。
func clamavScanConn(conn net.Conn, r io.Reader) error {
	if _, err := conn.Write(clamavCommand); err != nil {
		return fmt.Errorf("clamav send command: %w", err)
	}
	if err := clamavWriteChunks(conn, r); err != nil {
		return fmt.Errorf("clamav send stream: %w", err)
	}
	resp, err := clamavReadResponse(conn)
	if err != nil {
		return fmt.Errorf("clamav read response: %w", err)
	}
	return parseClamAVResponse(resp)
}

// clamavWriteChunks 按协议分块写出全部内容，并以 0 长度块终止。
func clamavWriteChunks(w io.Writer, r io.Reader) error {
	buf := make([]byte, clamavChunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := clamavWriteChunk(w, buf[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return clamavWriteChunk(w, nil)
}

// clamavWriteChunk 写入单个 INSTREAM 帧：4 字节大端长度 + 数据（纯函数）。
func clamavWriteChunk(w io.Writer, data []byte) error {
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(data)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	_, err := w.Write(data)
	return err
}

// clamavReadResponse 读取应答直到 \0 或连接关闭，应答异常超长时报错。
func clamavReadResponse(conn net.Conn) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 256)
	for {
		n, err := conn.Read(tmp)
		buf.Write(tmp[:n])
		if bytes.IndexByte(tmp[:n], 0) >= 0 {
			break
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if buf.Len() > 4096 {
			return nil, errors.New("response exceeds 4096 bytes")
		}
	}
	return buf.Bytes(), nil
}

// parseClamAVResponse 解析 clamd 应答（纯函数）。
func parseClamAVResponse(resp []byte) error {
	msg := strings.TrimRight(string(resp), "\x00\r\n \t")
	if msg == "" {
		return errors.New("clamav: empty response")
	}
	switch {
	case strings.HasSuffix(msg, " OK") || msg == "OK":
		return nil
	case strings.HasSuffix(msg, " FOUND"):
		virus := strings.TrimSpace(strings.TrimSuffix(msg, " FOUND"))
		virus = strings.TrimSpace(strings.TrimPrefix(virus, "stream:"))
		if virus == "" {
			virus = msg
		}
		return fmt.Errorf("%w: %s", ErrMalwareDetected, virus)
	default:
		return fmt.Errorf("clamav: unexpected response: %q", msg)
	}
}

// SelectScanner 按配置选择扫描器：
//   - 未启用扫描（SCAN_ENABLED=false）            -> AllowScanner
//   - 启用扫描但未配置 CLAMAV_ADDR                -> RejectScanner（fail closed）
//   - 启用扫描且配置地址                          -> ClamAVScanner
func SelectScanner(scanEnabled bool, clamavAddr string, required bool, timeout time.Duration) Scanner {
	if !scanEnabled {
		return AllowScanner{}
	}
	if clamavAddr == "" {
		return RejectScanner{}
	}
	return NewClamAVScanner(clamavAddr, timeout, required)
}
