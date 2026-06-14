# ---------- 构建阶段 ----------
FROM golang:1.26-alpine AS builder
WORKDIR /src

# 先拷贝 go.mod 以利用层缓存(本项目无第三方依赖,go.sum 可有可无)
COPY go.mod ./
RUN go mod download

COPY . .
# 静态编译,去符号表减小体积
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /revproxy .

# ---------- 运行阶段 ----------
FROM alpine:3.20
# 连接 HTTPS 上游需要根证书
RUN apk add --no-cache ca-certificates

# 抖音云 veFaaS 约定:平台固定执行 /opt/application/run.sh 启动服务,
# 二进制与启动脚本都放到 /opt/application 下。
WORKDIR /opt/application
COPY --from=builder /revproxy /opt/application/revproxy
# 直接在镜像内生成启动脚本,避免独立文件被漏提交到仓库
RUN printf '%s\n' \
      '#!/bin/sh' \
      'export PORT="${PORT:-8000}"' \
      'exec /opt/application/revproxy' \
      > /opt/application/run.sh \
 && chmod +x /opt/application/run.sh /opt/application/revproxy

# veFaaS 期望函数监听 8000 端口
ENV PORT=8000
EXPOSE 8000

# 本地 docker run 时用;veFaaS 会忽略此处而固定执行 run.sh
CMD ["/opt/application/run.sh"]
