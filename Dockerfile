# syntax=docker/dockerfile:1.7
#
# Logview 生产镜像。
#
# 三个阶段：
#   1. web      固定版本 Node 完成前端类型检查与构建（架构无关，只在构建平台跑一次）
#   2. gobuild  固定版本 Go，CGO_ENABLED=0 交叉编译到目标架构，内嵌上一步的产物
#   3. runtime  精简运行镜像，只含静态链接的二进制，不需要 Node/Python/ttyd/ccze/tmux
#
# 基础镜像同时固定 tag 与摘要（@sha256:...）。升级方式：改 ARG 的 tag 与摘要，
# 摘要可用 `docker buildx imagetools inspect <image>:<tag>` 取得。
#
# 注意：web 与 gobuild 阶段用 --platform=$BUILDPLATFORM 以构建平台原生速度运行
# （Go 交叉编译不需要 QEMU），只有 TARGETARCH/TARGETVARIANT 决定产物架构。

ARG NODE_IMAGE=node:24.20.0-alpine@sha256:e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf
ARG GO_IMAGE=golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb
ARG RUNTIME_IMAGE=alpine:3.22.2@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412

# ---------------------------------------------------------------- 前端构建
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS web

WORKDIR /src/web

# 先只拷贝清单，这样源码变化时依赖安装层仍能命中缓存。
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY web/ ./
# build 脚本先做完整类型检查（vue-tsc + tsc）再 vite build，失败会让镜像构建失败。
RUN npm run build

# ---------------------------------------------------------------- Go 编译
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS gobuild

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src

# 无第三方依赖，仍先拷 go.mod，保持缓存语义并显式声明模块路径。
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# 前端产物覆盖占位目录：只需生成一次，供所有架构复用。
RUN rm -rf internal/server/dist && mkdir -p internal/server/dist
COPY --from=web /src/web/dist/ ./internal/server/dist/

# TARGETVARIANT 只在 arm 上有意义（arm/v7 → GOARM=7）；其他架构留空。
RUN set -eu; \
    goarm=""; \
    case "${TARGETARCH}" in \
      arm) goarm="${TARGETVARIANT#v}" ;; \
    esac; \
    test -f internal/server/dist/index.html; \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" GOARM="${goarm}" \
      go build -trimpath -ldflags="-s -w" -o /out/logview ./cmd/logview

# ---------------------------------------------------------------- 运行镜像
FROM ${RUNTIME_IMAGE} AS runtime

ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="Logview" \
      org.opencontainers.image.description="只读的 fakessh 日志查看器（Go + Vue，单进程）" \
      org.opencontainers.image.source="https://github.com/Seameee/Logview" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="MIT"

# 独立 UID/GID：在宿主上把日志文件授权给这个组（受限 ACL 或属组），
# 而不是用 chmod 777、特权模式或去掉 no-new-privileges。
#
# 这里直接用数字 UID/GID，不执行 adduser/addgroup：静态链接的 Go 二进制不需要
# /etc/passwd 条目。省掉这条 RUN 之后，运行镜像不再要求构建平台能执行目标架构的
# 指令，三架构交叉构建因此不需要 QEMU。
COPY --from=gobuild /out/logview /usr/local/bin/logview

# 运行期默认值。FILTER_WORDS 有意不设默认值：旧镜像曾隐含 LeasewebSG，见 README 迁移说明。
ENV LOG_FILE=/var/log/0.log \
    ANONYMIZE_MODE=partial \
    LOG_FORMAT=auto \
    EXPOSE_LOG_DETAILS=false

USER 10001:10001
# 工作目录设为不存在的路径：应用不需要写任何东西，只读根文件系统下也没有可写位置。
WORKDIR /nonexistent

EXPOSE 7681

# healthz 只表示进程存活，源状态在响应体的 source 字段里单独体现。
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:7681/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/logview"]
