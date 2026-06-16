package newapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 上游调用步骤名(入日志 + 选平台码;绝不返前端)。
const (
	stepCreateUser  = "CreateUser"
	stepLogin       = "Login"
	stepGetToken    = "GetToken"
	stepCreateToken = "CreateToken"
	stepRevealKey   = "RevealTokenKey"
	stepDeleteToken = "DeleteToken"
	stepGetUser     = "GetUser"
	stepManageUser  = "ManageUserQuota"
	stepSetStatus   = "SetUserStatus"
	stepProbe       = "ProbeAccessToken"
)

// Config 配置 adapter 的 Executor 单出口行为。零值有合理默认(见 normalize)。
type Config struct {
	BaseURL     string // new-api 基址,如 http://localhost:13000(无尾斜杠)
	AdminToken  string // 管理员 access_token,用于 AdminAuth/RootAuth 调用
	AdminUserID int    // 管理员 user_id,作 New-Api-User 头

	Timeout      time.Duration // 单次请求短超时(默认 8s)——绝不长挂,保护 new-api 与平台
	MaxRetries   int           // 可重试错误的最大重试次数(默认 2,即最多 3 次尝试)
	BackoffBase  time.Duration // 退避基数(默认 200ms,指数 + 抖动)
	RateLimitQPS float64       // 对上游的整体限速(默认 20 QPS)——绝不全量打 new-api
	RateBurst    int           // 令牌桶突发(默认 10)
	CBThreshold  int           // 连续失败多少次熔断打开(默认 5)
	CBCooldown   time.Duration // 熔断打开后多久转半开(默认 10s)

	// Logger 可选结构化日志钩子。adapter 只传脱敏字段(绝不传 token/key/password,见 10 §4.2)。
	Logger func(level, event string, kv map[string]any)
}

func (c *Config) normalize() {
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.Timeout <= 0 {
		c.Timeout = 8 * time.Second
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 2
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 200 * time.Millisecond
	}
	if c.RateLimitQPS <= 0 {
		c.RateLimitQPS = 20
	}
	if c.RateBurst <= 0 {
		c.RateBurst = 10
	}
	if c.CBThreshold <= 0 {
		c.CBThreshold = 5
	}
	if c.CBCooldown <= 0 {
		c.CBCooldown = 10 * time.Second
	}
}

// client 是上游单出口:所有对 new-api 的请求都过它(限速/退避/熔断/短超时)。
type client struct {
	cfg     Config
	httpC   *http.Client
	limiter *tokenBucket
	cb      *circuitBreaker
	rng     *rand.Rand
	rngMu   sync.Mutex
}

func newClient(cfg Config) *client {
	cfg.normalize()
	return &client{
		cfg:     cfg,
		httpC:   &http.Client{Timeout: cfg.Timeout},
		limiter: newTokenBucket(cfg.RateLimitQPS, cfg.RateBurst),
		cb:      newCircuitBreaker(cfg.CBThreshold, cfg.CBCooldown),
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (c *client) log(level, event string, kv map[string]any) {
	if c.cfg.Logger != nil {
		c.cfg.Logger(level, event, kv)
	}
}

// authMode 描述一次上游调用怎么带鉴权。三种互斥:
//   - admin:Bearer AdminToken + New-Api-User AdminUserID(CreateUser/ManageUser/option/channel)
//   - user:Bearer cred.AccessToken + New-Api-User cred.NewapiUserID(令牌 CRUD,按持有者作用域)
//   - session/none:登录拿 cookie / 无鉴权
type authMode struct {
	bearer     string
	newAPIUser string
	cookie     string // 原始 Cookie 头(session 子流程用)
	none       bool
}

func adminAuth(cfg Config) authMode {
	return authMode{bearer: cfg.AdminToken, newAPIUser: strconv.Itoa(cfg.AdminUserID)}
}

func userAuth(cred MemberCred) authMode {
	return authMode{bearer: cred.AccessToken, newAPIUser: strconv.Itoa(cred.NewapiUserID)}
}

// sessionAuth 用于 login 后的 GET /api/user/token:rc.4 即使带 session cookie
// 也强制要 New-Api-User 头(05 §1.2 实证),故两者都带。
func sessionAuth(cookie string, userID int) authMode {
	return authMode{cookie: cookie, newAPIUser: strconv.Itoa(userID)}
}

func noAuth() authMode { return authMode{none: true} }

// envelope 是 new-api 的标准响应信封。注意:new-api 常以 HTTP 200 + success:false
// 表达业务失败,故不能只看 HTTP 状态码(05 §5)。
type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// callResult 是一次成功上游调用的结果。
type callResult struct {
	data      json.RawMessage
	setCookie string // 来自响应的 session cookie(login 用)
}

// do 执行一次上游调用,内置限速 + 熔断 + 退避重试 + 短超时 + 信封解析。
// 返回 *UpstreamError(已脱敏、含平台码)或成功结果。
func (c *client) do(ctx context.Context, step, method, path string, auth authMode, body any) (*callResult, *UpstreamError) {
	// 1) 熔断闸:打开则直接拒,不打上游。
	if !c.cb.allow() {
		c.log("WARN", "upstream.circuit_open", map[string]any{"step": step})
		return nil, ErrCircuitOpen
	}

	// 2) 限速:等到有令牌或 ctx 取消。
	if err := c.limiter.wait(ctx); err != nil {
		return nil, newTransportError(step, isTimeout(err), err)
	}

	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, &UpstreamError{Step: step, PlatformCode: CodeInternal, Message: "请求序列化失败", class: classNonRetryable, cause: err}
		}
		payload = b
	}

	attempts := c.cfg.MaxRetries + 1
	var lastErr *UpstreamError
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if !c.sleepBackoff(ctx, attempt) {
				return nil, newTransportError(step, true, ctx.Err())
			}
		}
		res, uerr := c.doOnce(ctx, step, method, path, auth, payload)
		if uerr == nil {
			c.cb.onSuccess()
			return res, nil
		}
		lastErr = uerr
		// 仅对可重试错误计入熔断 + 继续重试;不可重试(4xx/鉴权)立即返回。
		if uerr.Retryable() {
			c.cb.onFailure()
			c.log("WARN", "upstream.retryable_error", map[string]any{
				"step": step, "attempt": attempt + 1, "http": uerr.HTTPStatus, "code": uerr.PlatformCode,
			})
			continue
		}
		return nil, uerr
	}
	return nil, lastErr
}

func (c *client) doOnce(ctx context.Context, step, method, path string, auth authMode, payload []byte) (*callResult, *UpstreamError) {
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	var bodyReader io.Reader
	if payload != nil {
		bodyReader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, c.cfg.BaseURL+path, bodyReader)
	if err != nil {
		return nil, &UpstreamError{Step: step, PlatformCode: CodeInternal, Message: "构造请求失败", class: classNonRetryable, cause: err}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !auth.none {
		if auth.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+auth.bearer)
		}
		if auth.newAPIUser != "" {
			// 命门:UserAuth 即使带 cookie 也强制要 New-Api-User 头(05 §1.2 / §5.2)。
			req.Header.Set("New-Api-User", auth.newAPIUser)
		}
		if auth.cookie != "" {
			req.Header.Set("Cookie", auth.cookie)
		}
	}

	resp, err := c.httpC.Do(req)
	if err != nil {
		to := isTimeout(err)
		c.log("WARN", "upstream.transport_error", map[string]any{"step": step, "timeout": to})
		return nil, newTransportError(step, to, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 限读 1MB,防异常大响应

	// HTTP 层非 2xx:按状态码分类(5xx 可重试 / 4xx 不可 / 401 自愈)。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyHTTP(step, resp.StatusCode, "")
	}

	// HTTP 2xx:解析信封。new-api 用 success:false 表达业务失败。
	var env envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			// 非标准信封(极少数端点直接返数据);宽松处理,把整个 body 当 data。
			env = envelope{Success: true, Data: json.RawMessage(raw)}
		}
	} else {
		env = envelope{Success: true}
	}
	if !env.Success {
		// 业务失败:不可重试。鉴权类关键词触发自愈分类。
		if looksLikeAuthFailure(env.Message) {
			e := classifyHTTP(step, http.StatusUnauthorized, "上游鉴权失效")
			return nil, e
		}
		e := &UpstreamError{Step: step, HTTPStatus: resp.StatusCode, class: classNonRetryable, PlatformCode: platformCodeForBiz(step), Message: "上游拒绝该请求", upstreamMsg: env.Message}
		c.log("WARN", "upstream.biz_reject", map[string]any{"step": step, "code": e.PlatformCode})
		return nil, e
	}

	return &callResult{data: env.Data, setCookie: extractSessionCookie(resp.Header)}, nil
}

func (c *client) sleepBackoff(ctx context.Context, attempt int) bool {
	// 指数退避 + 抖动:base * 2^(attempt-1) * [0.5,1.5)
	d := c.cfg.BackoffBase * time.Duration(1<<(attempt-1))
	c.rngMu.Lock()
	jitter := 0.5 + c.rng.Float64()
	c.rngMu.Unlock()
	d = time.Duration(float64(d) * jitter)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func looksLikeAuthFailure(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "access token") || strings.Contains(m, "unauthorized") ||
		strings.Contains(m, "无权") || strings.Contains(m, "登录") || strings.Contains(m, "token invalid")
}

// extractSessionCookie 从响应头取 session cookie,拼成可回发的 Cookie 头值。
func extractSessionCookie(h http.Header) string {
	var parts []string
	for _, sc := range h.Values("Set-Cookie") {
		// 只取 "name=value" 段,丢掉 Path/Expires 等属性。
		if i := strings.Index(sc, ";"); i >= 0 {
			sc = sc[:i]
		}
		sc = strings.TrimSpace(sc)
		if sc != "" {
			parts = append(parts, sc)
		}
	}
	return strings.Join(parts, "; ")
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) {
		return te.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// q 拼接 query 参数到 path。
func q(path string, kv map[string]string) string {
	if len(kv) == 0 {
		return path
	}
	vals := url.Values{}
	for k, v := range kv {
		vals.Set(k, v)
	}
	return path + "?" + vals.Encode()
}
