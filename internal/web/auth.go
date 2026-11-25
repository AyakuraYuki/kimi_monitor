package web

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json/v2"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"kimi_balance_monitor/internal/config"
)

const sessionLifetime = 8 * time.Hour

type webSession struct {
	origin   string
	username string
	csrf     string
	expires  time.Time
}

type loginWindow struct {
	count   int
	expires time.Time
}

type loginReservation struct {
	key     string
	expires time.Time
}

type Auth struct {
	cfg       config.WebConfig
	mu        sync.Mutex
	sessions  map[string]webSession
	attempts  map[string]loginWindow
	pending   map[string]oidcLogin
	hashSlots chan struct{}
	oidc      oidcClient
}

func NewAuth(cfg config.WebConfig) (*Auth, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Auth{
		cfg:      cfg,
		sessions: make(map[string]webSession), attempts: make(map[string]loginWindow),
		pending: make(map[string]oidcLogin), hashSlots: make(chan struct{}, 4),
		oidc: newOIDCClient(),
	}, nil
}

// Register installs the origin policy before the application adds its routes.
func (a *Auth) Register(r *gin.Engine) {
	r.Use(a.guardOrigin)
	r.GET("/api/session", a.sessionInfo)
	r.POST("/api/login", a.login)
	r.POST("/api/logout", a.Require(), a.logout)
	r.GET("/auth/oidc/start", a.startOIDC)
	r.GET("/auth/oidc/callback", a.finishOIDC)
}

func (a *Auth) guardOrigin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "DENY")
	c.Header("Referrer-Policy", "same-origin")
	origin := externalOrigin(c.Request)
	if origin == "" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "访问地址或协议无效，请通过 HTTPS 代理或本机回环地址访问"})
		return
	}
	c.Set(originKey, origin)
	if strings.HasPrefix(origin, "https://") {
		c.Header("Strict-Transport-Security", "max-age=31536000")
	}
	if unsafeMethod(c.Request.Method) && (!strings.EqualFold(c.GetHeader("Origin"), origin) || c.GetHeader("Sec-Fetch-Site") == "cross-site") {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "请求来源无效，请从控制台重试"})
		return
	}
	c.Next()
}

func unsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func decodeJSON(c *gin.Context, dst any) bool {
	contentType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || contentType != "application/json" {
		c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "请求需要使用 JSON 格式"})
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "无法读取请求或内容过长"})
		return false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' || json.Unmarshal(body, dst, json.RejectUnknownMembers(true)) != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "请求字段或 JSON 格式无效"})
		return false
	}
	return true
}

func (a *Auth) current(c *gin.Context) (webSession, bool) {
	id, err := c.Cookie(sessionCookieName(c))
	if err != nil || id == "" {
		return webSession{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if ok && !s.expires.After(time.Now()) {
		delete(a.sessions, id)
		return webSession{}, false
	}
	return s, ok && s.origin == requestOrigin(c)
}

func (a *Auth) Require() gin.HandlerFunc {
	return func(c *gin.Context) {
		s, ok := a.current(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "请先登录控制台"})
			return
		}
		if unsafeMethod(c.Request.Method) && subtle.ConstantTimeCompare([]byte(c.GetHeader("X-CSRF-Token")), []byte(s.csrf)) != 1 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "请求校验失败，请刷新页面后重试"})
			return
		}
		c.Next()
	}
}

func (a *Auth) sessionInfo(c *gin.Context) {
	s, ok := a.current(c)
	c.JSON(http.StatusOK, a.sessionPayload(s, ok))
}

func (a *Auth) sessionPayload(s webSession, ok bool) gin.H {
	payload := gin.H{"authenticated": ok, "local_enabled": a.cfg.PasswordHash != "", "oidc_enabled": a.cfg.OIDC.Issuer != ""}
	if ok {
		payload["username"] = s.username
		payload["csrf_token"] = s.csrf
	}
	return payload
}

func (a *Auth) setCookie(c *gin.Context, name, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   strings.HasPrefix(requestOrigin(c), "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) newSession(c *gin.Context, username string) (webSession, bool) {
	now := time.Now()
	s := webSession{
		origin:   requestOrigin(c),
		username: username,
		csrf:     rand.Text(),
		expires:  now.Add(sessionLifetime),
	}
	id := rand.Text()
	old, _ := c.Cookie(sessionCookieName(c))
	a.mu.Lock()
	defer a.mu.Unlock()
	if previous, ok := a.sessions[old]; ok && previous.origin == s.origin {
		delete(a.sessions, old)
	}
	maps.DeleteFunc(a.sessions, func(_ string, v webSession) bool { return !v.expires.After(now) })
	if len(a.sessions) >= 4096 {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "当前会话较多，请稍后重试"})
		return webSession{}, false
	}
	a.sessions[id] = s
	a.setCookie(c, sessionCookieName(c), id, int(sessionLifetime.Seconds()))
	return s, true
}

func (a *Auth) logout(c *gin.Context) {
	id, _ := c.Cookie(sessionCookieName(c))
	a.mu.Lock()
	delete(a.sessions, id)
	a.mu.Unlock()
	a.setCookie(c, sessionCookieName(c), "", -1)
	c.Status(http.StatusNoContent)
}

func (a *Auth) reserveLogin(c *gin.Context, method string) (loginReservation, bool) {
	// Count the actual TCP peer. An untrusted forwarding header must not reset this budget.
	peer, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		peer = c.Request.RemoteAddr
	}
	key := method + "/" + peer
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.attempts[key]
	if !w.expires.After(now) {
		maps.DeleteFunc(a.attempts, func(_ string, v loginWindow) bool { return !v.expires.After(now) })
		w = loginWindow{expires: now.Add(5 * time.Minute)}
	}
	if w.count >= 10 || (w.count == 0 && len(a.attempts) >= 4096) {
		c.Header("Retry-After", "300")
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "登录请求过于频繁，请稍后重试"})
		return loginReservation{}, false
	}
	w.count++
	a.attempts[key] = w
	return loginReservation{key: key, expires: w.expires}, true
}

// A successful sign-in returns only its own slot, preserving other failed attempts
// from the same peer (including other users behind the same reverse proxy).
func (a *Auth) refundLogin(slot loginReservation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.attempts[slot.key]
	if w.expires.Equal(slot.expires) && w.count > 0 {
		w.count--
		if w.count == 0 {
			delete(a.attempts, slot.key)
		} else {
			a.attempts[slot.key] = w
		}
	}
}

func (a *Auth) login(c *gin.Context) {
	if a.cfg.PasswordHash == "" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "未启用本地登录"})
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(c, &input) {
		return
	}
	slot, ok := a.reserveLogin(c, "local")
	if !ok {
		return
	}
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		a.refundLogin(slot)
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "登录请求较多，请稍后重试"})
		return
	}
	// Evaluate the expensive hash even for an unknown username.
	hashErr := bcrypt.CompareHashAndPassword([]byte(a.cfg.PasswordHash), []byte(input.Password))
	userMatches := subtle.ConstantTimeCompare([]byte(input.Username), []byte(a.cfg.Username))
	if hashErr != nil || userMatches != 1 || len(input.Password) > 72 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}
	if s, ok := a.newSession(c, a.cfg.Username); ok {
		a.refundLogin(slot)
		c.JSON(http.StatusOK, a.sessionPayload(s, true))
	}
}
