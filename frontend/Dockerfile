# caddy 镜像：前端 SPA 静态产物 + draw.io 静态层 + Caddy 配置。
# 前端不在容器内构建（无 node/npm ci/tsc 阶段）：产物 dist/ 由本地
# `npm run build` 生成后直接 COPY——规避 BuildKit 在 WSL2 内做高内存
# 编译（antd6 类型检查曾致 JS heap OOM 与 Docker Desktop 引擎僵死），
# 构建耗时也从 ~2min 降到秒级。部署脚本须先跑本地构建（deploy_all.sh）。
# 注意：frontend/.dockerignore 排除 src/node_modules，仅放行 dist 等产物。

FROM jgraph/drawio:27.0.9 AS drawio
USER root
RUN mkdir /drawio-static \
    && cp -a /usr/local/tomcat/webapps/draw/. /drawio-static/ \
    && rm -rf /drawio-static/WEB-INF

FROM caddy:2.8.4-alpine
COPY dist /srv/frontend
COPY --from=drawio /drawio-static/ /srv/drawio/
