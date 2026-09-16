#!/bin/bash
# 绕过 backend：直接向 caddy admin API 下发含 default_sni 的 internal 配置，
# 验证 IP 站点 https 握手（等效验证 default_sni 修复；backend 渲染已有单测覆盖）。
set -e
cat > /tmp/conf.caddy <<'EOF'
{
	admin {$CADDY_ADMIN:localhost:2019}
	default_sni 127.0.0.1
}

127.0.0.1 {
	tls internal
	encode gzip
	handle /health {
		respond 200
	}
	handle {
		root * /srv/frontend
		try_files {path} /index.html
		file_server
	}
	log {
		output stdout
	}
}
EOF
docker cp /tmp/conf.caddy docflow-caddy-1:/tmp/conf.caddy >/dev/null
docker exec docflow-caddy-1 sh -c 'wget --header="Content-Type: text/caddyfile" --post-file=/tmp/conf.caddy -qO- http://localhost:2019/load && echo LOAD_OK'
sleep 2
echo '--- https handshake ---'
curl -sk -m 5 -o /dev/null -w 'https front: %{http_code}\n' https://127.0.0.1/ && echo TLS_HANDSHAKE_OK || echo TLS_FAIL
curl -sk -m 5 -o /dev/null -w 'https health: %{http_code}\n' https://127.0.0.1/health
echo '--- cert subject ---'
echo | openssl s_client -connect 127.0.0.1:443 2>/dev/null | openssl x509 -noout -subject -issuer 2>/dev/null
echo '--- restore http mode ---'
docker exec docflow-caddy-1 sh -c 'wget --header="Content-Type: text/caddyfile" --post-file=/etc/caddy/Caddyfile -qO- http://localhost:2019/load' && echo RESTORED
sleep 2
curl -s -m 5 -o /dev/null -w 'http front: %{http_code}\n' http://127.0.0.1/
echo DIRECT_VERIFY_DONE
