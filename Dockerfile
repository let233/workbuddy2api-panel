# ---------- 前端构建（React + Vite） ----------
# 前端产物不再打进 Go 二进制：Go 侧运行时把 public/ 目录映射成 /panel/ 静态资源
# （internal/panel/index.go）。所以镜像里需要这一阶段，输出 public/ 供运行镜像复制。
FROM node:22-alpine AS web
WORKDIR /src/web
# 先只拷依赖清单：源码改动不会让 npm ci 这层缓存失效
COPY web/package.json web/package-lock.json ./
# npmmirror：境内构建直连 registry.npmjs.org 经常卡住；境外可用 --build-arg 覆盖。
ARG NPM_REGISTRY=https://registry.npmmirror.com
RUN npm ci --no-audit --no-fund --registry=${NPM_REGISTRY}
COPY web/ ./
# vite 的 outDir 是 ../public，产物落在 /src/public
RUN npm run build

# ---------- 后端构建 ----------
FROM golang:1.26.10-alpine AS build
# Go module 代理：默认官方源（境外/CI 不多绕一跳）；境内或镜像环境用
# --build-arg 覆盖，容器内直连 proxy.golang.org 在境内会长时间卡住：
#   docker compose build --build-arg GOPROXY=https://goproxy.cn,direct
# 也可写进 .env（compose 会把 .env 变量当 build-arg 传入）。
# direct 兜底：代理未收录的模块仍能直接回源，不会因缺包而构建失败。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG APP_VERSION=dev
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.appVersion=${APP_VERSION}" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
# /app 放宽到 0777：面板「关于 → 立即更新」要在容器里原地替换 /app/wb2api，
# 换文件只需要目录写权限，而 compose 允许 PUID/PGID 覆盖成宿主机 uid（镜像默认
# 10001:10001）。容器内所有进程本来就是同一个用户，放宽目录权限不扩大权限面。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app \
 && chmod 0777 /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
# 前端产物：/app/public 由 Go 侧映射到 /panel/（默认 panel.public_dir = ./public）
COPY --from=web /src/public /app/public
COPY login.sh signin.sh credit.sh /app/
COPY scripts/probe_active.py /app/scripts/probe_active.py
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
