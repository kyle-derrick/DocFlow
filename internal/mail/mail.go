// Package mail 提供系统邮件发送抽象（邀请注册、密码重置）。
// 实现：
//   - NoopMailer（默认）：不发送，仅日志输出链接（SMTP 未配置/禁用时使用）；
//   - SMTPMailer：net/smtp 实现（auto=STARTTLS 自动协商 / ssl=隐式 TLS /
//     none=明文），支持运行时参数；
//   - SettingsMailer：每次发送时读取当前生效配置（system_settings 入库值
//     覆盖 env，见 internal/settings 的 smtp.* 键）动态装配 SMTPMailer，
//     禁用时回退 fallback（Noop）——管理端改 SMTP 配置即时生效，无需重启。
package mail

import (
	"crypto/tls"
	"fmt"
	"log"
	"mime"
	"net"
	"net/smtp"
	"time"
)

// TLS 模式（与 settings.SMTPTLSMode* 常量一致，独立定义避免包依赖）。
const (
	TLSModeAuto = "auto"
	TLSModeSSL  = "ssl"
	TLSModeNone = "none"
)

// Mailer 抽象系统邮件：链接（邀请/重置）均为一次性明文 token 的完整 URL，
// 由调用方拼接（PUBLIC_BASE_URL + 前端路由）。
type Mailer interface {
	// SendInvitation 发送邀请注册邮件（inviter 为邀请人用户名，可为空）。
	SendInvitation(email, link, inviter string) error
	// SendPasswordReset 发送密码重置邮件（链接默认 30 分钟有效且仅可使用一次）。
	SendPasswordReset(email, link string) error
	// SendNotification 发送站内通知事件的邮件副本（纯文本简单格式）。
	SendNotification(email, title, body string) error
	// SendEmailChangeCode 发送换绑邮箱验证码（账号安全，v2.4）：username
	// 为请求账号的用户名；验证码 10 分钟有效。
	SendEmailChangeCode(email, code, username string) error
}

// NoopMailer 不发送任何邮件，仅把链接写入日志（默认邮件通道）。
type NoopMailer struct{}

// NewNoopMailer 构造日志型邮件通道。
func NewNoopMailer() *NoopMailer { return &NoopMailer{} }

func (NoopMailer) SendInvitation(email, link, inviter string) error {
	log.Printf("[mail:noop] invitation for %s (invited by %s): %s", email, inviter, link)
	return nil
}

func (NoopMailer) SendPasswordReset(email, link string) error {
	log.Printf("[mail:noop] password reset for %s: %s", email, link)
	return nil
}

func (NoopMailer) SendNotification(email, title, body string) error {
	log.Printf("[mail:noop] notification for %s: %s", email, title)
	return nil
}

func (NoopMailer) SendEmailChangeCode(email, code, username string) error {
	log.Printf("[mail:noop] email change code for %s (user %s): %s", email, username, code)
	return nil
}

// SMTPMailer 为 net/smtp 实现：单条纯文本邮件、可选 PLAIN 认证（user 为
// 空不认证）。TLS 模式：auto（默认）在服务器通告 STARTTLS 时自动协商；
// ssl 直接 TLS 拨号（SMTPS/465）；none 不协商（仅可信内网中继）。
type SMTPMailer struct {
	host    string
	port    int
	user    string
	pass    string
	from    string
	tlsMode string
}

// NewSMTPMailer 构造 SMTP 邮件通道（host:port，发件人 from，可选账号密码；
// tlsMode 空 = auto）。
func NewSMTPMailer(host string, port int, user, pass, from, tlsMode string) *SMTPMailer {
	if tlsMode == "" {
		tlsMode = TLSModeAuto
	}
	return &SMTPMailer{host: host, port: port, user: user, pass: pass, from: from, tlsMode: tlsMode}
}

func (s *SMTPMailer) SendInvitation(email, link, inviter string) error {
	body := fmt.Sprintf("您好，\n\n%s 邀请您加入 DocFlow。请在邀请有效期内点击以下链接完成注册（链接仅可使用一次）：\n\n%s\n\n若链接无法点击，请将其复制到浏览器地址栏打开。", inviter, link)
	return s.send(email, "DocFlow 邀请注册", body)
}

func (s *SMTPMailer) SendPasswordReset(email, link string) error {
	body := fmt.Sprintf("您好，\n\n您（或他人）请求重置 DocFlow 账号密码。以下链接 30 分钟内有效且仅可使用一次：\n\n%s\n\n如非本人操作，请忽略本邮件，您的密码不会被更改。", link)
	return s.send(email, "DocFlow 密码重置", body)
}

// SendNotification 发送站内通知事件的邮件副本（纯文本简单格式：
// 标题 + 正文 + 偏好调整指引）。
func (s *SMTPMailer) SendNotification(email, title, body string) error {
	text := fmt.Sprintf("您好，\n\n%s\n\n%s\n\n—— DocFlow（可在「设置 → 通知偏好」中调整各类通知开关）", title, body)
	return s.send(email, "[DocFlow] "+title, text)
}

// SendEmailChangeCode 发送换绑邮箱验证码（账号安全）：10 分钟内有效，
// 仅对请求中提交的新邮箱投递（新邮箱归属验证即由本邮件完成）。
func (s *SMTPMailer) SendEmailChangeCode(email, code, username string) error {
	body := fmt.Sprintf("您好 %s，\n\n您正在更换 DocFlow 账号的绑定邮箱。验证码：\n\n%s\n\n10 分钟内有效。若非本人操作，请立即检查账号安全（修改密码并撤销活跃会话）。\n\n—— DocFlow", username, code)
	return s.send(email, "DocFlow 换绑邮箱验证码", body)
}

// send 组装最简 RFC 5322 报文并投递（显式 smtp.Client：按 TLS 模式拨号，
// STARTTLS 在 MAIL 命令前协商）。
func (s *SMTPMailer) send(to, subject, body string) error {
	msg := []byte("From: " + s.from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" +
		body + "\r\n")
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	// 拨号带 30s 超时：SMTP 服务器不可达时快速失败（管理页「发送测试邮件」
	// 等同步请求不得无限挂起），超时错误原样回传给调用方。
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	var conn net.Conn
	var err error
	if s.tlsMode == TLSModeSSL {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return err
	}
	defer client.Close()
	if s.tlsMode != TLSModeNone {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: s.host}); err != nil {
				return err
			}
		}
	}
	if s.user != "" {
		if err := client.Auth(smtp.PlainAuth("", s.user, s.pass, s.host)); err != nil {
			return err
		}
	}
	if err := client.Mail(s.from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// SMTPConfig 为一次投递所需的全部 SMTP 参数（SettingsMailer 的源数据）。
type SMTPConfig struct {
	Host    string
	Port    int
	User    string
	Pass    string
	From    string
	TLSMode string
}

// SMTPSource 每次发送时读取当前生效配置（system_settings 入库覆盖回退
// env 的合并结果）；第二返回值 false = 通道禁用（回退 fallback）。
type SMTPSource func() (SMTPConfig, bool)

// SettingsMailer 按当前生效配置动态投递：source 禁用时走 fallback，启用时
// 就地装配 SMTPMailer（邮件频次低，逐次装配成本可忽略；换来配置热生效）。
type SettingsMailer struct {
	source   SMTPSource
	fallback Mailer
}

// NewSettingsMailer 构造动态邮件通道（source 为配置源，fallback 通常为
// NoopMailer）。
func NewSettingsMailer(source SMTPSource, fallback Mailer) *SettingsMailer {
	return &SettingsMailer{source: source, fallback: fallback}
}

// route 返回本次投递应使用的邮件器：禁用回退 fallback，启用就地装配。
func (m *SettingsMailer) route() Mailer {
	cfg, ok := m.source()
	if !ok {
		return m.fallback
	}
	return NewSMTPMailer(cfg.Host, cfg.Port, cfg.User, cfg.Pass, cfg.From, cfg.TLSMode)
}

func (m *SettingsMailer) SendInvitation(email, link, inviter string) error {
	return m.route().SendInvitation(email, link, inviter)
}

func (m *SettingsMailer) SendPasswordReset(email, link string) error {
	return m.route().SendPasswordReset(email, link)
}

func (m *SettingsMailer) SendNotification(email, title, body string) error {
	return m.route().SendNotification(email, title, body)
}

func (m *SettingsMailer) SendEmailChangeCode(email, code, username string) error {
	return m.route().SendEmailChangeCode(email, code, username)
}

var _ Mailer = (*NoopMailer)(nil)
var _ Mailer = (*SMTPMailer)(nil)
var _ Mailer = (*SettingsMailer)(nil)
