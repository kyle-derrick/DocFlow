// 自定义证书（custom 模式）的存储与解析：管理员上传 PEM 证书+私钥，
// 落盘到证书目录（compose 中为 backend/caddy 共享卷，backend 可写、
// caddy 只读），Apply(custom) 渲染的 tls 指令经 CADDY_TLS_CERT/KEY
// 环境变量指向该路径。写入为原子替换（临时文件 + rename），权限 0600。
package caddytls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/google/uuid"
)

// 证书目录内的固定文件名（与 compose 的 CADDY_TLS_CERT/KEY 默认值一致）。
const (
	certFileName = "cert.pem"
	keyFileName  = "key.pem"
)

// CertInfo 为已存证书的解析摘要（管理页展示：CN / 有效期 / SAN）。
type CertInfo struct {
	CN        string    `json:"cn"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	DNSNames  []string  `json:"dns_names,omitempty"`
}

// certPaths 返回证书/私钥文件的绝对路径。
func (s *Service) certPaths() (cert, key string) {
	return filepath.Join(s.certDir, certFileName), filepath.Join(s.certDir, keyFileName)
}

// parseCertPEM 解析 PEM 证书链（取首张叶子证书做摘要）。
func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	var block *pem.Block
	rest := pemBytes
	for {
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("certificate PEM not found (expect -----BEGIN CERTIFICATE-----)")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

// parseKeyPEM 解析 PEM 私钥（PKCS#1 RSA / PKCS#8 / EC / Ed25519）。
func parseKeyPEM(pemBytes []byte) (any, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("private key PEM not found (expect -----BEGIN ... PRIVATE KEY-----)")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		return x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported private key PEM block %q", block.Type)
	}
}

// publicKeyOf 提取私钥对应的公钥（证书匹配校验用）。
func publicKeyOf(key any) (any, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey, nil
	case *ecdsa.PrivateKey:
		return &k.PublicKey, nil
	case ed25519.PrivateKey:
		return k.Public(), nil
	default:
		return nil, errors.New("unsupported private key type")
	}
}

// validatePair 校验证书与私钥可解析且公钥匹配。
func validatePair(certPEM, keyPEM []byte) (*x509.Certificate, error) {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	key, err := parseKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	certPub, err := publicKeyOf(key)
	if err != nil {
		return nil, err
	}
	if !publicKeysEqual(cert.PublicKey, certPub) {
		return nil, errors.New("private key does not match certificate")
	}
	return cert, nil
}

// publicKeysEqual 比较两把公钥是否同一把：x509.MarshalPKIXPublicKey 输出
// 规范 DER，按字节比较（覆盖 RSA/ECDSA/Ed25519 全类型；不能走
// interface{ Equal(any) bool } 断言——各公钥的 Equal 形参为命名类型
// crypto.PublicKey，与方法集 any 形参签名不同，断言恒失败）。
func publicKeysEqual(a, b any) bool {
	derA, errA := x509.MarshalPKIXPublicKey(a)
	derB, errB := x509.MarshalPKIXPublicKey(b)
	return errA == nil && errB == nil && bytes.Equal(derA, derB)
}

// writeFileAtomic 原子写入（临时文件 + rename），权限 0600。
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tls-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后为 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// UploadCert 校验并保存 PEM 证书+私钥（原子替换旧证书），返回新证书摘要；
// 成功后写 tls.cert_upload 审计（不落证书内容）。
func (s *Service) UploadCert(certPEM, keyPEM []byte, actor string) (CertInfo, error) {
	cert, err := validatePair(certPEM, keyPEM)
	if err != nil {
		return CertInfo{}, err
	}
	certPath, keyPath := s.certPaths()
	if err := writeFileAtomic(certPath, certPEM); err != nil {
		return CertInfo{}, fmt.Errorf("write certificate: %w", err)
	}
	if err := writeFileAtomic(keyPath, keyPEM); err != nil {
		return CertInfo{}, fmt.Errorf("write private key: %w", err)
	}
	info := CertInfoFromCert(cert)
	if s.audit != nil {
		var by *uuid.UUID
		if id, perr := uuid.Parse(actor); perr == nil {
			by = &id
		}
		metadata, _ := json.Marshal(map[string]string{"cn": info.CN, "not_after": info.NotAfter.Format(time.RFC3339)})
		_ = s.audit.Record(audit.Entry{UserID: by, Action: "tls.cert_upload", ResourceType: "caddy", ResourceID: "tls", Status: audit.StatusSuccess, Metadata: string(metadata)})
	}
	return info, nil
}

// ReadCert 读取已存证书的摘要；目录为空 / 文件缺失 / 解析失败返回错误
// （custom 模式未就绪即 Apply 前置校验与管理页展示均消费该错误）。
func (s *Service) ReadCert() (CertInfo, error) {
	certPath, _ := s.certPaths()
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return CertInfo{}, fmt.Errorf("custom certificate not uploaded: %w", err)
	}
	cert, err := parseCertPEM(raw)
	if err != nil {
		return CertInfo{}, fmt.Errorf("stored certificate unreadable: %w", err)
	}
	return CertInfoFromCert(cert), nil
}

// CertInfoFromCert 构造证书摘要（SAN 去重保持原顺序）。
func CertInfoFromCert(cert *x509.Certificate) CertInfo {
	names := make([]string, 0, len(cert.DNSNames))
	seen := make(map[string]bool, len(cert.DNSNames))
	for _, n := range cert.DNSNames {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return CertInfo{
		CN:        cert.Subject.CommonName,
		NotBefore: cert.NotBefore,
		NotAfter:  cert.NotAfter,
		DNSNames:  names,
	}
}
