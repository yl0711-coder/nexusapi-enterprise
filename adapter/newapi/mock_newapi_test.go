package newapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// fakeNewapi 是一个 httptest 级别的 rc.4 契约模拟器,复刻 05 §5 实证过的关键行为:
//   - CreateUser 重名 → 200 + success:false "用户名已存在"(驱动 §2.5 接管)
//   - GET /api/user/token 一调即旋转:每次返回新 access_token,旧的立即失效(§2.6 命门)
//   - 令牌列表脱敏、明文须经 GetTokenKey 单取(05 §5.2)
//   - UserAuth 强制 Bearer + New-Api-User 双头(05 §1.2)
//   - ManageUser override 写绝对值 / enable / disable(05 §1.1)
//
// 同时支持按步骤注入 5xx / 空 token 故障,用以测 §2.2 失败矩阵与补偿。
type fakeNewapi struct {
	mu          sync.Mutex
	srv         *httptest.Server
	adminToken  string
	adminUserID int
	users       map[string]*fakeUser // by username
	byID        map[int]*fakeUser
	sessions    map[string]string // cookie value -> username
	nextUserID  int
	nextTokenID int
	nextSession int

	// 故障注入(剩余触发次数 / 开关)
	faultCreateUser5xx  int
	faultGetToken5xx    int
	faultGetTokenEmpty  bool
	faultCreateToken5xx int
	faultRevealKey5xx   int

	calls map[string]int // 步骤调用计数(断言用)
}

type fakeUser struct {
	id            int
	username      string
	password      string
	enabled       bool
	quota         int64
	currentAccess string // 当前有效 access_token;GET token 旋转后此值更新,旧值即失效
	tokens        map[int]*fakeToken
}

type fakeToken struct {
	id   int
	name string
	key  string
}

func newFakeNewapi() *fakeNewapi {
	f := &fakeNewapi{
		adminToken:  "admin-access-token",
		adminUserID: 1,
		users:       map[string]*fakeUser{},
		byID:        map[int]*fakeUser{},
		sessions:    map[string]string{},
		nextUserID:  100,
		nextTokenID: 5000,
		calls:       map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/", f.handleUser)        // POST create / PUT update / GET search 落到这或下面
	mux.HandleFunc("/api/user/search", f.handleSearch) // GET search
	mux.HandleFunc("/api/user/login", f.handleLogin)
	mux.HandleFunc("/api/user/token", f.handleGetToken)
	mux.HandleFunc("/api/user/manage", f.handleManage)
	mux.HandleFunc("/api/token/", f.handleToken) // 含 /api/token/, /api/token/:id, /api/token/:id/key
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeNewapi) close() { f.srv.Close() }

func (f *fakeNewapi) url() string { return f.srv.URL }

func (f *fakeNewapi) config() Config {
	return Config{
		BaseURL:     f.url(),
		AdminToken:  f.adminToken,
		AdminUserID: f.adminUserID,
		MaxRetries:  2,
		// 测试里把退避压到极小,避免拖慢。
		BackoffBase: 1,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "", "data": data})
}

func bizFail(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": msg, "data": nil})
}

func (f *fakeNewapi) bump(step string) {
	f.calls[step]++
}

func (f *fakeNewapi) callCount(step string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[step]
}

// requireAdmin 校验 AdminAuth(Bearer admin + New-Api-User adminID)。
func (f *fakeNewapi) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+f.adminToken {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "message": "unauthorized"})
		return false
	}
	return true
}

// requireUser 校验 UserAuth(Bearer access + New-Api-User id 双头),且 access 必须是该用户当前有效值。
// 返回该用户;失败已写响应。
func (f *fakeNewapi) requireUser(w http.ResponseWriter, r *http.Request) *fakeUser {
	auth := r.Header.Get("Authorization")
	napiUser := r.Header.Get("New-Api-User")
	if !strings.HasPrefix(auth, "Bearer ") || napiUser == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "message": "缺少 New-Api-User 或 Authorization 头"})
		return nil
	}
	access := strings.TrimPrefix(auth, "Bearer ")
	uid, _ := strconv.Atoi(napiUser)
	u := f.byID[uid]
	if u == nil || u.currentAccess != access {
		// 旋转作废 / 不匹配 → access token invalid(触发自愈)。
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "access token invalid"})
		return nil
	}
	return u
}

func (f *fakeNewapi) handleUser(w http.ResponseWriter, r *http.Request) {
	// /api/user/search 已单独注册;这里只处理精确 /api/user/ 的 POST(CreateUser)。
	if r.URL.Path != "/api/user/" {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	f.bump(stepCreateUser)
	if f.requireAdmin(w, r) == false {
		return
	}
	if f.faultCreateUser5xx > 0 {
		f.faultCreateUser5xx--
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "internal"})
		return
	}
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Username == "" {
		bizFail(w, "用户名不能为空")
		return
	}
	if _, exists := f.users[body.Username]; exists {
		bizFail(w, "用户名已存在")
		return
	}
	f.nextUserID++
	u := &fakeUser{id: f.nextUserID, username: body.Username, password: body.Password, enabled: true, tokens: map[int]*fakeToken{}}
	f.users[body.Username] = u
	f.byID[u.id] = u
	ok(w, map[string]any{"id": u.id})
}

func (f *fakeNewapi) handleSearch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bump(stepGetUser)
	if !f.requireAdmin(w, r) {
		return
	}
	kw := r.URL.Query().Get("keyword")
	var list []map[string]any
	for _, u := range f.users {
		if kw == "" || strings.Contains(u.username, kw) {
			list = append(list, map[string]any{"id": u.id, "username": u.username})
		}
	}
	ok(w, list)
}

func (f *fakeNewapi) handleLogin(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bump(stepLogin)
	var body struct{ Username, Password string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	u := f.users[body.Username]
	if u == nil || u.password != body.Password {
		bizFail(w, "用户名或密码错误")
		return
	}
	if !u.enabled {
		bizFail(w, "用户已被禁用")
		return
	}
	f.nextSession++
	cookie := fmt.Sprintf("sess-%d", f.nextSession)
	f.sessions[cookie] = u.username
	http.SetCookie(w, &http.Cookie{Name: "session", Value: cookie, Path: "/"})
	ok(w, map[string]any{"id": u.id, "username": u.username})
}

func (f *fakeNewapi) handleGetToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bump(stepGetToken)
	if f.faultGetToken5xx > 0 {
		f.faultGetToken5xx--
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "bad gateway"})
		return
	}
	c, err := r.Cookie("session")
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "message": "no session"})
		return
	}
	username := f.sessions[c.Value]
	u := f.users[username]
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "message": "no session"})
		return
	}
	if f.faultGetTokenEmpty {
		ok(w, "") // 空 access_token:测 §2.2 step③ 不可重试
		return
	}
	// 一调即旋转:生成新 access_token,旧的立即失效。
	u.currentAccess = fmt.Sprintf("acc-%s-%d", u.username, f.nextSession)
	ok(w, u.currentAccess)
}

func (f *fakeNewapi) handleManage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.requireAdmin(w, r) {
		return
	}
	var body struct {
		ID     int    `json:"id"`
		Action string `json:"action"`
		Mode   string `json:"mode"`
		Quota  int64  `json:"quota"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	u := f.byID[body.ID]
	if u == nil {
		bizFail(w, "用户不存在")
		return
	}
	switch body.Action {
	case "add_quota":
		f.bump(stepManageUser)
		switch body.Mode {
		case "override":
			u.quota = body.Quota
		case "add":
			u.quota += body.Quota
		case "subtract":
			u.quota -= body.Quota
		default:
			bizFail(w, "未知 mode")
			return
		}
	case "enable":
		f.bump(stepSetStatus)
		u.enabled = true
	case "disable":
		f.bump(stepSetStatus)
		u.enabled = false
	default:
		bizFail(w, "未知 action")
		return
	}
	ok(w, map[string]any{"id": u.id, "quota": u.quota, "enabled": u.enabled})
}

func (f *fakeNewapi) handleToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rest := strings.TrimPrefix(r.URL.Path, "/api/token/")

	switch {
	case rest == "" && r.Method == http.MethodGet: // 列表
		u := f.requireUser(w, r)
		if u == nil {
			return
		}
		var list []map[string]any
		for _, t := range u.tokens {
			list = append(list, map[string]any{"id": t.id, "name": t.name, "key": maskKey(t.key)})
		}
		ok(w, list)
	case rest == "" && r.Method == http.MethodPost: // 建 token
		f.bump(stepCreateToken)
		if f.faultCreateToken5xx > 0 {
			f.faultCreateToken5xx--
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "internal"})
			return
		}
		u := f.requireUser(w, r)
		if u == nil {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.nextTokenID++
		t := &fakeToken{id: f.nextTokenID, name: body.Name, key: fmt.Sprintf("sk-nexus-%d-secret", f.nextTokenID)}
		u.tokens[t.id] = t
		ok(w, map[string]any{"id": t.id, "name": t.name})
	case strings.HasSuffix(rest, "/key") && r.Method == http.MethodPost: // 取明文 key
		f.bump(stepRevealKey)
		if f.faultRevealKey5xx > 0 {
			f.faultRevealKey5xx--
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "internal"})
			return
		}
		u := f.requireUser(w, r)
		if u == nil {
			return
		}
		idStr := strings.TrimSuffix(rest, "/key")
		id, _ := strconv.Atoi(idStr)
		t := u.tokens[id]
		if t == nil {
			bizFail(w, "token 不存在")
			return
		}
		ok(w, t.key)
	case r.Method == http.MethodDelete: // 删 token
		f.bump(stepDeleteToken)
		u := f.requireUser(w, r)
		if u == nil {
			return
		}
		id, _ := strconv.Atoi(rest)
		if _, exists := u.tokens[id]; !exists {
			bizFail(w, "token 不存在")
			return
		}
		delete(u.tokens, id)
		ok(w, nil)
	default:
		http.NotFound(w, r)
	}
}

func maskKey(k string) string {
	if len(k) <= 4 {
		return "****"
	}
	return "sk-nexus-••••" + k[len(k)-4:]
}
