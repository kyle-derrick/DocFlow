package caddytls

import "strings"

// Render 生成完整 Caddyfile 文本：与 deploy/Caddyfile 同构（裸机部署参考），
// 仅主站地址与 TLS 指令按 mode/domain 参数化；其余 {$VAR} 占位符原样下发、
// 由 caddy 容器 env 展开。修改 deploy/Caddyfile 的站点规则时须同步本模板
// （caddytls_test.go 的模板锚定断言可拦截遗漏）。
//
// admin 块：CADDY_ADMIN env 控制 admin API 监听（compose 内网下发通道；
// reload 后配置内必须保留本块，否则 admin 回落 localhost 不可达）。
func Render(mode Mode, domain string) string {
	site := "{$APP_DOMAIN::80}" // http：env 驱动（本地验证默认，明文）
	tlsLine := ""
	defaultSNI := ""
	switch mode {
	case ModeAuto:
		site = domain // Caddy 自动 ACME（HTTP-01 挑战走 80，自动 308 重定向）
	case ModeInternal:
		site = domain
		tlsLine = "\ttls internal\n"
		// IP 站点（如 127.0.0.1/192.168.x.x）：客户端对 IP 不发送 SNI，
		// Caddy 无 SNI 时证书匹配失败（TLS internal error alert）。全局
		// default_sni 兜底到本站点后握手成功（域名站点亦无害）。
		defaultSNI = strings.Split(domain, ":")[0]
	case ModeCustom:
		site = domain
		// 自定义证书：tls 指令指向共享卷内的证书/私钥文件。路径经
		// CADDY_TLS_CERT/KEY env 由 caddy 容器展开（compose 已挂载同一
		// 卷并设置默认值 /data/tls/cert.pem、/data/tls/key.pem）。
		tlsLine = "\ttls {$CADDY_TLS_CERT:/data/tls/cert.pem} {$CADDY_TLS_KEY:/data/tls/key.pem}\n"
		// 同 internal：custom 证书亦允许 IP 站点，default_sni 兜底握手。
		defaultSNI = strings.Split(domain, ":")[0]
	}

	var b strings.Builder
	b.WriteString("{\n\tadmin {$CADDY_ADMIN:localhost:2019}\n")
	if defaultSNI != "" {
		b.WriteString("\tdefault_sni " + defaultSNI + "\n")
	}
	b.WriteString("}\n\n")
	b.WriteString(site + " {\n")
	if tlsLine != "" {
		b.WriteString(tlsLine)
	}
	b.WriteString(`	encode gzip

	# 安全响应头基线（CSP 由应用层提供，Caddy 不重复下发）。
	header {
		X-Content-Type-Options nosniff
		Referrer-Policy same-origin
	}

	handle /health {
		respond 200
	}
	handle /ready {
		reverse_proxy backend:8080
	}

	handle /api/* {
		reverse_proxy backend:8080
	}

	handle /content/* {
		header {
			-Cookie
			Content-Security-Policy "sandbox allow-scripts"
			X-Content-Type-Options nosniff
		}
		reverse_proxy {$CONTENT_UPSTREAM:backend:8080} {
			header_up -Cookie
		}
	}

	handle /raw/* {
		header {
			-Cookie
			Content-Security-Policy "sandbox allow-scripts"
			X-Content-Type-Options nosniff
			Referrer-Policy no-referrer
		}
		reverse_proxy {$CONTENT_UPSTREAM:backend:8080} {
			header_up -Cookie
		}
	}

	handle /mcp {
		reverse_proxy {$CONTENT_UPSTREAM:backend:8080}
	}

	handle_path /onlyoffice/* {
		reverse_proxy {$ONLYOFFICE_UPSTREAM:127.0.0.1:9} {
			header_up X-Forwarded-Path /onlyoffice
		}
	}

	@drawioEditor path_regexp drawioEditor ^/drawio/[0-9a-fA-F-]{36}$
	handle @drawioEditor {
		root * /srv/frontend
		try_files {path} /index.html
		file_server
	}

	handle_path /drawio/* {
		root * /srv/drawio
		header Cache-Control "public, max-age=86400"
		try_files {path} /index.html
		file_server
	}

	# /assets/* 为带内容哈希的前端产物，永久缓存；其余 SPA 路径（html/
	# sw.js/manifest）发版即变，no-cache 每次验证。
	handle /assets/* {
		root * /srv/frontend
		header Cache-Control "public, max-age=31536000, immutable"
		file_server
	}
	handle {
		root * /srv/frontend
		try_files {path} /index.html
		header Cache-Control "no-cache"
		file_server
	}

	log {
		output stdout
	}
}

{$CONTENT_DOMAIN:content.localhost} {
	encode gzip
	header {
		-Cookie
		Content-Security-Policy "sandbox allow-scripts"
		X-Content-Type-Options nosniff
	}
	reverse_proxy /content/* {$CONTENT_UPSTREAM:backend:8080} {
		header_up -Cookie
	}
	respond / 404
}
`)
	return b.String()
}
