package handler

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// 进程内轻量指标(R2-运维:无 /metrics)。纯标准库原子计数,不引第三方依赖;
// Prometheus 文本格式暴露。MVP 够用:总量 + 按状态段 + 在途 + 启动时刻。
var (
	metricReqTotal   atomic.Int64 // 累计请求数
	metricReq2xx     atomic.Int64
	metricReq4xx     atomic.Int64
	metricReq5xx     atomic.Int64
	metricInflight   atomic.Int64 // 在途请求
	metricStartUnix  atomic.Int64 // 进程启动 Unix 秒(首次访问惰性置位由 main 注入更佳,这里惰性兜底)
)

// statusRecorder 包装 ResponseWriter 以捕获状态码与响应字节数(访问日志/指标用)。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// accessLog 记录每请求一行(method/path/status/耗时/request_id),并累计指标。
// 跳过静态前端与探针噪声(/healthz、/readyz、/metrics 不记访问日志,但仍计数指标)。
func (h *Handler) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		metricInflight.Add(1)
		next.ServeHTTP(rec, r)
		metricInflight.Add(-1)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		metricReqTotal.Add(1)
		switch {
		case rec.status >= 500:
			metricReq5xx.Add(1)
		case rec.status >= 400:
			metricReq4xx.Add(1)
		case rec.status >= 200 && rec.status < 300:
			metricReq2xx.Add(1)
		}

		// 探针/指标自身不刷访问日志,避免噪声淹没真实请求。
		switch r.URL.Path {
		case "/healthz", "/readyz", "/metrics":
			return
		}
		h.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur_ms", time.Since(start).Milliseconds(),
			"bytes", rec.bytes,
			"request_id", requestIDFrom(r.Context()),
		)
	})
}

// handleMetrics 暴露 Prometheus 文本格式指标(无需鉴权;部署内网/被监控抓取)。
func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	uptime := int64(0)
	if s := metricStartUnix.Load(); s > 0 {
		uptime = time.Now().Unix() - s
	}
	fmt.Fprintf(w, "# HELP nexus_http_requests_total 累计处理的 HTTP 请求数(按状态段)\n")
	fmt.Fprintf(w, "# TYPE nexus_http_requests_total counter\n")
	fmt.Fprintf(w, "nexus_http_requests_total %d\n", metricReqTotal.Load())
	fmt.Fprintf(w, "nexus_http_requests_total{class=\"2xx\"} %d\n", metricReq2xx.Load())
	fmt.Fprintf(w, "nexus_http_requests_total{class=\"4xx\"} %d\n", metricReq4xx.Load())
	fmt.Fprintf(w, "nexus_http_requests_total{class=\"5xx\"} %d\n", metricReq5xx.Load())
	fmt.Fprintf(w, "# HELP nexus_http_inflight 当前在途请求数\n")
	fmt.Fprintf(w, "# TYPE nexus_http_inflight gauge\n")
	fmt.Fprintf(w, "nexus_http_inflight %d\n", metricInflight.Load())
	fmt.Fprintf(w, "# HELP nexus_uptime_seconds 进程运行时长(秒)\n")
	fmt.Fprintf(w, "# TYPE nexus_uptime_seconds gauge\n")
	fmt.Fprintf(w, "nexus_uptime_seconds %d\n", uptime)
}

// markStarted 记录进程启动时刻(main 在装配时调用)。
func markStarted(unix int64) { metricStartUnix.Store(unix) }
