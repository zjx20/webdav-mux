# syntax=docker/dockerfile:1

# 多架构构建：编译阶段固定运行在构建机的原生平台（BUILDPLATFORM）上，靠 Go 的交叉编译
# 产出目标平台（TARGETPLATFORM）的二进制，因此构建任何架构的镜像都不需要 QEMU 模拟。
# 运行阶段基于 scratch：镜像里只有一个静态链接的二进制和 CA 证书。

ARG GO_VERSION=1.27.1

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src

# 依赖清单单独成层：只改代码时不必重新下载依赖。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# TARGETOS、TARGETARCH、TARGETVARIANT 由构建工具按 --platform 为每个目标平台设置。
ARG TARGETOS TARGETARCH TARGETVARIANT
# - CGO_ENABLED=0：静态链接，scratch 里没有 libc 也能运行；
# - -trimpath：不把构建机上的源码路径写进二进制；
# - -ldflags="-s -w"：去掉符号表和 DWARF 调试信息，二进制约小三分之一。panic 的栈追踪
#   依赖 Go 自己的 pclntab，函数名和行号照常输出；代价只是不能再用 dlv/gdb 调试这个二进制。
# 平台变体映射到 Go 的对应设置，例如 linux/arm/v6 -> GOARM=6，linux/amd64/v3 -> GOAMD64=v3。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    case "$TARGETARCH" in \
      arm) export GOARM="${TARGETVARIANT#v}" ;; \
      amd64) export GOAMD64="${TARGETVARIANT:-v1}" ;; \
    esac && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w" -o /out/webdav-mux .

FROM scratch

# 访问 HTTPS 上游时校验服务器证书需要 CA 证书。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/webdav-mux /webdav-mux

# 以非 root 用户运行。scratch 里没有 /etc/passwd，只能写数字 ID；
# 挂载进容器的配置文件和本地目录需要对这个 UID 可读，否则用 docker run --user 覆盖。
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/webdav-mux"]
CMD ["serve", "-config", "/etc/webdav-mux/config.yaml"]
