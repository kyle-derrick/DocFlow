# DocFlow 前端构建 + Caddy 入口镜像（生产，设计文档 §12）：
# 阶段 1（node:22-alpine）：npm ci（缓存 ./.npm-cache）→ vite build 产出 dist；
# 阶段 2（caddy:2.8.4-alpine）：仅拷贝静态产物 dist → /srv/frontend，由挂载的
# deploy/Caddyfile 服务——前端只由 Caddy 提供静态产物，不运行 dev server。
#
# 方案取舍：前端产物并入 caddy 镜像（而非独立 frontend 服务 + caddy 反代）——
# 少一个容器与一跳内部转发，前端无任何运行时依赖，构建期合并最简单。
FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --cache ./.npm-cache
COPY . .
RUN npm run build

FROM caddy:2.8.4-alpine
COPY --from=build /app/dist /srv/frontend
