// 一个把本地请求反向代理到 https://xxx.nextlnk1.com/ 的简单反向代理。
//
// 用法:
//
//	go run .                       # 监听 :8080，转发到默认上游
//	go run . -listen :9000         # 自定义监听地址
//	go run . -target https://other.example.com/
//	go run . -insecure             # 跳过上游 TLS 证书校验(自签名时用)
//
// 启动后访问 http://localhost:8080/ 即可,所有请求(含路径、查询串、
// 请求体、方法、头部)都会被转发到上游,响应原样返回。
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	// 默认值支持被环境变量覆盖,方便在 PaaS / 容器平台上部署
	// (这类平台通常注入 PORT,且不便传命令行参数)。
	listen := flag.String("listen", envOr("LISTEN", ":"+envOr("PORT", "8080")), "本地监听地址")
	target := flag.String("target", envOr("TARGET", "https://xxx.nextlnk1.com/"), "上游目标地址")
	insecure := flag.Bool("insecure", envBool("INSECURE"), "跳过上游 TLS 证书校验")
	preserveHost := flag.Bool("preserve-host", envBool("PRESERVE_HOST"), "保留客户端原始 Host 头(默认改写为上游 Host)")
	flag.Parse()

	// 若 listen 只是个纯端口号(平台常见做法),补上冒号。
	if !strings.Contains(*listen, ":") {
		*listen = ":" + *listen
	}

	upstream, err := url.Parse(*target)
	if err != nil {
		log.Fatalf("无效的 target 地址 %q: %v", *target, err)
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		log.Fatalf("target 必须是完整 URL,例如 https://host/,当前: %q", *target)
	}

	proxy := newProxy(upstream, *insecure, *preserveHost)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           withLogging(proxy),
		ReadHeaderTimeout: 15 * time.Second,
		// 不设置 WriteTimeout,以兼容长连接/流式响应(如 SSE、下载)。
	}

	// 优雅退出。
	go func() {
		log.Printf("反向代理已启动: %s  ->  %s", *listen, upstream.String())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务异常退出: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("正在关闭...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("关闭超时: %v", err)
	}
}

func newProxy(upstream *url.URL, insecure, preserveHost bool) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(upstream)

	// 自定义传输层:控制 TLS、连接池、超时。
	proxy.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: insecure, // 默认 false;-insecure 时为 true
			ServerName:         upstream.Hostname(),
		},
	}

	// NewSingleHostReverseProxy 已经设置了 Director 来改写 scheme/host/path,
	// 这里在它的基础上包一层,补充 Host 头与转发头。
	orig := proxy.Director
	proxy.Director = func(req *http.Request) {
		orig(req)
		if !preserveHost {
			// 让上游收到正确的 Host(对基于域名的虚拟主机/SNI 很重要)。
			req.Host = upstream.Host
		}
		// 标准转发头,便于上游识别真实客户端与协议。
		req.Header.Set("X-Forwarded-Host", req.Header.Get("Host"))
		req.Header.Set("X-Forwarded-Proto", schemeOf(req))
		// 避免把本机的 gzip 偏好强加给上游导致二次解压问题,保持透明。
	}

	// 改写响应:把上游下发的重定向/Cookie 域名指回代理,避免跳出代理。
	proxy.ModifyResponse = func(resp *http.Response) error {
		rewriteRedirect(resp, upstream)
		return nil
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("代理出错 %s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, "上游不可达: "+err.Error(), http.StatusBadGateway)
	}

	return proxy
}

// rewriteRedirect 把 3xx 响应里指向上游主机的 Location 改成相对路径,
// 这样浏览器跟随跳转时仍然走代理,而不是直连上游。
func rewriteRedirect(resp *http.Response, upstream *url.URL) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return
	}
	u, err := url.Parse(loc)
	if err != nil {
		return
	}
	if u.Host == upstream.Host {
		u.Scheme = ""
		u.Host = ""
		resp.Header.Set("Location", u.String())
	}
}

func schemeOf(req *http.Request) string {
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

// withLogging 打印每个请求的基本信息。
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s %s (%s)",
			clientIP(r), r.Method, r.Host, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
	})
}

// envOr 返回环境变量 key 的值,为空时返回 def。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool 当环境变量为 1/true/yes/on(忽略大小写)时返回 true。
func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
