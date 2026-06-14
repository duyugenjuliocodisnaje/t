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
# 连接 HTTPS 上游需要根证书;再建个非 root 用户运行
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
USER app

COPY --from=builder /revproxy /usr/local/bin/revproxy

# 平台会注入 PORT;这里给个默认值,程序会监听 :$PORT
ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/revproxy"]
