FROM golang:1.22-alpine AS build
WORKDIR /src
# 国内网络构建可覆盖：docker compose build --build-arg GOPROXY=https://goproxy.cn
ARG GOPROXY=
ENV GOPROXY=${GOPROXY:-https://proxy.golang.org}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/docflow ./cmd/server
RUN CGO_ENABLED=0 go build -o /out/migrate ./cmd/migrate
RUN CGO_ENABLED=0 go build -o /out/seed ./cmd/seed

FROM alpine:3.20
RUN adduser -D -H app \
	# named volume 首挂载以镜像目录属主初始化：存储目录必须归 app，
	# 否则 /ready 的 storage 写探针失败（root 属主只读）。
	&& mkdir -p /data/storage && chown app:app /data/storage
USER app
COPY --from=build /out/docflow /docflow
COPY --from=build /out/migrate /migrate
COPY --from=build /out/seed /seed
# 迁移 SQL 随镜像分发，供独立 migrate 服务在每次部署时执行。
COPY --chown=app:root migrations /migrations
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
	CMD wget -qO- http://localhost:8080/ready || exit 1
ENTRYPOINT ["/docflow"]
