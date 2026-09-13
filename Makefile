.PHONY: run test fmt seed migrate e2e build-frontend up-minimal up-full down

run:
	go run ./cmd/server

test:
	go test ./...

fmt:
	gofmt -w $$(go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./...)

seed:
	go run ./cmd/seed

# 数据库迁移：按文件名序执行 migrations/*.sql（脚本内建幂等，可重复执行；
# 单文件失败即退出）。需先设置 DATABASE_URL，例如：
#   DATABASE_URL=postgres://docflow:change-me@localhost:5432/docflow?sslmode=disable make migrate
migrate:
	go run ./cmd/migrate

# Playwright E2E（frontend/e2e，需真实 PostgreSQL，本机无 Docker 时请先自备库）。
# 前置条件：
#   1) PostgreSQL 16 可达并已建库（示例，数据卷可复用）：
#        docker run -d --name docflow-e2e-pg -p 5432:5432 \
#          -e POSTGRES_DB=docflow -e POSTGRES_USER=docflow \
#          -e POSTGRES_PASSWORD=change-me postgres:16-alpine
#   2) 导出 E2E_DATABASE_URL（必填，无则 webServer fail fast 并打印指引）：
#        export E2E_DATABASE_URL="postgres://docflow:change-me@localhost:5432/docflow?sslmode=disable"
#      可选覆盖：E2E_JWT_SECRET、E2E_SEED_EMAIL、E2E_SEED_PASSWORD、E2E_STORAGE_ROOT。
#   3) 首次运行需安装测试浏览器：
#        cd frontend && npx playwright install chromium
# 说明：make e2e 会由 Playwright 自动拉起后端（先执行迁移与 seed，见
#   frontend/e2e/setup-backend.mjs）与 vite dev server（:5173），结束后自动回收。
e2e:
	cd frontend && npm run e2e

# 构建 caddy 入口镜像（多阶段：前端 vite 构建产物并入 caddy 镜像，
# 见 frontend/Dockerfile；即 docker compose 中 caddy 服务所用的镜像）。
build-frontend:
	docker compose build caddy

# 极简部署（caddy + backend + postgres，caddy 唯一宿主端口 80/443）。
up-minimal:
	docker compose --profile minimal up -d --build

# 完整部署（minimal + redis + onlyoffice；注入 ONLYOFFICE 反代上游，
# 否则 caddy 在 onlyoffice 未运行时不解析该主机名）。
up-full:
	ONLYOFFICE_UPSTREAM=onlyoffice:80 docker compose --profile full up -d --build

# 叠加病毒扫描：docker compose --profile minimal --profile antivirus up -d
#（或 --profile full --profile antivirus），并设 .env SCAN_ENABLED=true、
# CLAMAV_ADDR=clamav:3310。

# 停止全部 profile 的服务并移除容器（数据卷保留；需清数据再接 -v）。
down:
	docker compose --profile minimal --profile full --profile antivirus down
