# 多阶段构建:编译在容器内完成(绝不在生产机 build,铁律 §3)。
# 本机/CI 编好 → push 私有 GHCR → 生产只 pull + up。

# ---- build 阶段 ----
FROM golang:1.23-alpine AS build
WORKDIR /src
# 先拷 go.mod 利用层缓存(本项目里程碑 0 无外部依赖,go.sum 可缺省)。
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=0.0.0-dev
# 静态编译,产物可塞进 scratch/distroless。
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/server ./cmd/server

# ---- runtime 阶段 ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
