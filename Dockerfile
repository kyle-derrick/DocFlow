FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/docflow ./cmd/server

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/docflow /docflow
# 迁移 SQL 随镜像分发：schema 初建由 postgres initdb 挂载完成（见
# docker-compose.yml），后续增量迁移可从 /migrations 显式执行（psql/migrate）。
COPY --chown=app:root migrations /migrations
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
	CMD wget -qO- http://localhost:8080/ready || exit 1
ENTRYPOINT ["/docflow"]
