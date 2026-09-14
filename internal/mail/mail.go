// Package mail 提供系统邮件发送抽象（邀请注册、密码重置）。
// 两种实现：
//   - NoopMailer（默认）：不发送，仅日志输出链接（SMTP 未配置/禁用时使用）；
//   - SMTPMailer：net/smtp 简单实现（SMTP_ENABLED=true 时装配，禁用时不建立任何连接）。
package mail

import (
	"fmt"
	"log"
	"mime"
	"net/smtp"
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

// SMTPMailer 为 net/smtp 简单实现：单条纯文本邮件、可选 PLAIN 认证
// （user 为空不认证）；服务端通告 STARTTLS 时 net/smtp 自动协商。
type SMTPMailer struct {
	host string
	port int
	user string
	pass string
	from string
}

// NewSMTPMailer 构造 SMTP 邮件通道（host:port，发件人 from，可选账号密码）。
func NewSMTPMailer(host string, port int, user, pass, from string) *SMTPMailer {
	return &SMTPMailer{host: host, port: port, user: user, pass: pass, from: from}
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

// send 组装最简 RFC 5322 报文并经 smtp.SendMail 投递。
func (s *SMTPMailer) send(to, subject, body string) error {
	msg := []byte("From: " + s.from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" +
		body + "\r\n")
	var auth smtp.Auth
	if s.user != "" {
		auth = smtp.PlainAuth("", s.user, s.pass, s.host)
	}
	return smtp.SendMail(fmt.Sprintf("%s:%d", s.host, s.port), auth, s.from, []string{to}, msg)
}

var _ Mailer = (*NoopMailer)(nil)
var _ Mailer = (*SMTPMailer)(nil)
