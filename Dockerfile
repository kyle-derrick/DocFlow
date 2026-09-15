FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/docflow ./cmd/server
RUN CGO_ENABLED=0 go build -o /out/migrate ./cmd/migrate

FROM alpine:3.20
RUN adduser -D -H app
USER app
COPY --from=build /out/docflow /docflow
COPY --from=build /out/migrate /migrate
# 迁移 SQL 随镜像分发，供独立 migrate 服务在每次部署时执行。
COPY --chown=app:root migrations /migrations
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
	CMD wget -qO- http://localhost:8080/ready || exit 1
ENTRYPOINT ["/docflow"]
