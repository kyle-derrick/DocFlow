// Package http —— admin_env.go：启动级环境变量只读总览（配置总览页数据
// 源）。值取进程环境变量（即当前生效配置），未设置时展示代码内默认值；
// 敏感键（密钥/密码/令牌/盐）恒脱敏为 ••••，不回传真实值。修改启动级
// 配置需在部署文件（.env / docker-compose.yml）中调整并重启——本端点
// 只读，运行时设置请走 system_settings（平台管理各面板）。
package http

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

type envItemView struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Note      string `json:"note"`
	Sensitive bool   `json:"sensitive"`
}

type envGroupView struct {
	Name  string        `json:"name"`
	Items []envItemView `json:"items"`
}

// envSensitiveKey 判定是否敏感键（值恒脱敏）：名称含密钥/密码/令牌/
// 盐类词即视为敏感——宁可多脱敏，不泄露。
func envSensitiveKey(key string) bool {
	k := strings.ToUpper(key)
	for _, w := range []string{"SECRET", "PASSWORD", "PASS", "KEY", "TOKEN", "SALT"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// envCatalog 静态分组清单：key / 代码默认值 / 中文说明。仅收录核心键
//（完整清单见 docs/configuration.md）；默认值与 config.Load 保持一致。
var envCatalog = []struct {
	Name  string
	Items [][2]string // {key, 默认值或 ""}
	Notes map[string]string
}{
	{"服务与网络", [][2]string{
		{"PORT", "8080"}, {"APP_ENV", "development"}, {"PUBLIC_BASE_URL", ""},
		{"TRUSTED_PROXIES", ""}, {"ALLOWED_ORIGINS", ""}, {"COOKIE_DOMAIN", ""},
		{"CSRF_STRICT", "true"}, {"METRICS_ENABLED", "true"},
	}, map[string]string{
		"PUBLIC_BASE_URL": "对外基础地址（影响分享/OIDC 回调等）",
		"TRUSTED_PROXIES": "信任的反代 IP 列表（逗号分隔）",
		"ALLOWED_ORIGINS": "跨域白名单（逗号分隔）",
		"CSRF_STRICT":     "严格 CSRF 校验",
	}},
	{"数据库与缓存", [][2]string{
		{"DATABASE_URL", ""}, {"QUEUE_DRIVER", "inprocess"}, {"REDIS_ADDR", "localhost:6379"},
		{"REDIS_PASSWORD", ""}, {"QUEUE_CONCURRENCY", "4"}, {"BACKUP_DIR", ""},
	}, map[string]string{
		"DATABASE_URL": "PostgreSQL 连接串（必填）",
		"QUEUE_DRIVER": "inprocess | redis",
		"BACKUP_DIR":   "备份导出目录（留空禁用）",
	}},
	{"存储与上传", [][2]string{
		{"STORAGE_DRIVER", "local"}, {"STORAGE_ROOT", "./storage"}, {"MAX_FILE_SIZE", "536870912"},
		{"MAX_VERSIONS_PER_FILE", "20"}, {"S3_ENDPOINT", ""}, {"S3_BUCKET", ""},
		{"S3_REGION", "us-east-1"}, {"S3_PATH_STYLE", "true"}, {"S3_ACCESS_KEY", ""}, {"S3_SECRET_KEY", ""},
		{"CLAMAV_ADDR", ""}, {"CLAMAV_REQUIRED", "false"},
		{"CONTENT_PUBLIC_BASE_URL", ""}, {"RAW_URL_SECRET", ""},
	}, map[string]string{
		"STORAGE_DRIVER": "local | s3",
		"MAX_FILE_SIZE":  "单文件大小上限（字节）",
		"CLAMAV_ADDR":    "病毒扫描地址（留空跳过）",
		"RAW_URL_SECRET": "原始直链签名密钥",
	}},
	{"全文与向量检索", [][2]string{
		{"SEARCH_DRIVER", "pg"}, {"MEILI_URL", ""}, {"MEILI_API_KEY", ""},
		{"AI_RAG_VECTOR_ENABLED", "false"}, {"AI_RAG_QDRANT_URL", "http://qdrant:6333"},
		{"AI_RAG_MODE", "keyword"},
	}, map[string]string{
		"SEARCH_DRIVER":          "pg | meilisearch",
		"AI_RAG_VECTOR_ENABLED":  "向量检索开关（Qdrant profile）",
		"AI_RAG_QDRANT_URL":      "Qdrant 地址",
	}},
	{"Office 与图表", [][2]string{
		{"ONLYOFFICE_ENABLED", "false"}, {"ONLYOFFICE_SERVER_URL", "http://onlyoffice:80"},
		{"ONLYOFFICE_PUBLIC_URL", ""}, {"ONLYOFFICE_JWT_SECRET", ""},
		{"DRAWIO_ENABLED", "false"}, {"DRAWIO_SERVER_URL", "http://drawio:8080"},
		{"DRAWIO_PUBLIC_URL", ""},
	}, map[string]string{
		"ONLYOFFICE_PUBLIC_URL": "浏览器可达地址（留空按请求 Host 推导 /onlyoffice）",
		"DRAWIO_PUBLIC_URL":     "浏览器可达地址（留空按请求 Host 推导 /drawio）",
	}},
	{"AI Provider 引导", [][2]string{
		{"AI_ENABLED", "false"}, {"AI_BASE_URL", "https://api.openai.com/v1"},
		{"AI_API_KEY", ""}, {"AI_MODEL", "gpt-4o-mini"},
	}, map[string]string{
		"AI_ENABLED": "AI 总开关引导值（运行时可经平台管理覆盖）",
	}},
	{"Agent（Docker 沙箱）", [][2]string{
		{"DOCFLOW_AGENT_IPC_DIR", "/run/docflow-ipc"},
	}, map[string]string{
		"DOCFLOW_AGENT_IPC_DIR": "AI IPC socket 目录（named volume 挂载点）",
	}},
	{"安全与防爆破", [][2]string{
		{"JWT_SECRET", ""}, {"ACCESS_SALT", ""}, {"RATE_LIMIT_PER_MINUTE", "240"},
		{"PUBLIC_RATE_LIMIT_PER_MINUTE", "60"}, {"LOGIN_RATE_LIMIT_PER_MINUTE", "10"},
		{"LOGIN_MAX_RETRIES", "5"}, {"LOGIN_LOCK_MINUTES", "15"},
	}, map[string]string{
		"JWT_SECRET":        "JWT 签名密钥（≥32 字节）",
		"LOGIN_MAX_RETRIES": "登录/WebDAV 失败锁定阈值（按用户+IP）",
	}},
	{"邮件（SMTP）", [][2]string{
		{"SMTP_ENABLED", "false"}, {"SMTP_HOST", ""}, {"SMTP_PORT", "587"},
		{"SMTP_USER", ""}, {"SMTP_PASS", ""}, {"SMTP_FROM", ""},
	}, nil},
	{"TLS", [][2]string{
		{"TLS_CERT_DIR", "/data/tls"}, {"CADDY_ADMIN_ADDR", ""},
	}, nil},
	{"单点登录（OIDC）", [][2]string{
		{"OIDC_ENABLED", "false"}, {"OIDC_ISSUER", ""}, {"OIDC_CLIENT_ID", ""},
		{"OIDC_CLIENT_SECRET", ""}, {"OIDC_REDIRECT_URL", ""}, {"OIDC_AUTO_PROVISION", "true"},
	}, nil},
}

// adminGetEnv GET /api/v1/admin/settings/env：启动级环境变量只读总览
//（敏感值脱敏）。仅管理员组。
func (h *Handler) adminGetEnv(c *gin.Context) {
	groups := make([]envGroupView, 0, len(envCatalog))
	for _, g := range envCatalog {
		view := envGroupView{Name: g.Name, Items: make([]envItemView, 0, len(g.Items))}
		for _, kv := range g.Items {
			key, def := kv[0], kv[1]
			val := os.Getenv(key)
			sensitive := envSensitiveKey(key)
			if val == "" {
				val = def
			}
			if sensitive && val != "" {
				val = "••••"
			}
			note := ""
			if g.Notes != nil {
				note = g.Notes[key]
			}
			view.Items = append(view.Items, envItemView{Key: key, Value: val, Note: note, Sensitive: sensitive})
		}
		groups = append(groups, view)
	}
	c.JSON(http.StatusOK, gin.H{"groups": groups})
}
