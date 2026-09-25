# caddy 镜像：前端 SPA 静态产物 + draw.io 静态层 + Caddy 配置。
# 前端在镜像构建阶段完成 npm ci/build，因此 docker compose build 可直接从源码构建。

FROM node:22-alpine AS frontend-build
WORKDIR /src
ARG NPM_REGISTRY=https://registry.npmjs.org/
COPY package*.json ./
RUN npm config set registry "$NPM_REGISTRY" && npm ci
COPY . .
# tsc 类型检查 + vite 打包（antd6/monaco 等类型树）内存峰值高，默认堆会 OOM
# （exit 134）；与 CI workflows 一致固定 6GB 堆。
ENV NODE_OPTIONS=--max-old-space-size=6144
RUN npm run build

FROM jgraph/drawio:27.0.9 AS drawio
USER root
RUN mkdir /drawio-static \
    && cp -a /usr/local/tomcat/webapps/draw/. /drawio-static/ \
    && rm -rf /drawio-static/WEB-INF

FROM caddy:2.8.4-alpine
COPY --from=frontend-build /src/dist /srv/frontend
COPY --from=drawio /drawio-static/ /srv/drawio/
