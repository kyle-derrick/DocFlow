# DocFlow 前端构建 + Caddy 入口镜像（生产，设计文档 §12）：
# 阶段 1（node:22-alpine）：npm ci（缓存 ./.npm-cache）→ vite build 产出 dist；
# 阶段 2（jgraph/drawio）：仅取官方 webapp 纯静态层（排除 WEB-INF）；
# 阶段 3（caddy:2.8.4-alpine）：dist → /srv/frontend、draw.io 静态层 →
#   /srv/drawio，由挂载的 deploy/Caddyfile 服务——前端与图表编辑器都只由
#   Caddy 提供静态产物，不运行任何开发服务器。
#
# 方案取舍：
# - 前端产物并入 caddy 镜像（而非独立 frontend 服务 + caddy 反代）——少一个
#   容器与一跳内部转发，前端无任何运行时依赖，构建期合并最简单。
# - draw.io 同理并入：官方 webapp 为纯静态站（embed=1 iframe + postMessage
#   JSON 协议即自包含，本地编辑零后端依赖；官方镜像里的 Tomcat/Java 仅
#   服务其自建站云盘 OAuth 回调，本场景不使用——同源 iframe 下亦无跨源
#   问题）。对比原 jgraph/drawio 容器：少一个容器，代价是 caddy 镜像
FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --cache ./.npm-cache
COPY . .
RUN npm run build

FROM jgraph/drawio:27.0.9 AS drawio
USER root
RUN mkdir /drawio-static \
    && cp -a /usr/local/tomcat/webapps/draw/. /drawio-static/ \
    && rm -rf /drawio-static/WEB-INF

FROM caddy:2.8.4-alpine
COPY --from=build /app/dist /srv/frontend
COPY --from=drawio /drawio-static/ /srv/drawio/
