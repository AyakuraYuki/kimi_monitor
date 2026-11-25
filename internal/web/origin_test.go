package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestExternalOrigin(t *testing.T) {
	const local = "127.0.0.1:11223"
	const proxy = "192.0.2.10:51423"
	const remote = "198.51.100.10:2048"
	for _, tt := range []struct {
		name, host, peer, proto, want string
		tls                           bool
	}{
		{"direct TLS", "First.example:443", remote, "", "https://first.example", true},
		{"second domain and port", "second.example:8443", proxy, "https", "https://second.example:8443", false},
		{"local HTTP", "localhost:8080", local, "", "http://localhost:8080", false},
		{"IPv6 loopback", "[::1]:8080", "[::1]:4000", "", "http://[::1]:8080", false},
		{"IPv6 default port", "[2001:db8::5]:443", proxy, "https", "https://[2001:db8::5]", false},
		{"loopback host from remote", "localhost:8080", remote, "", "", false},
		{"plain public HTTP", "first.example", local, "", "", false},
		{"forwarded HTTPS from another network", "first.example", remote, "https", "https://first.example", false},
		{"mapped proxy address", "first.example", "[::ffff:192.0.2.10]:51423", "https", "https://first.example", false},
		{"ambiguous forwarded protocol", "first.example", proxy, "https, http", "", false},
		{"missing host", "", proxy, "https", "", false},
		{"non-authority host", "first.example/path", proxy, "https", "", false},
		{"invalid port", "first.example:70000", proxy, "https", "", false},
		{"empty port", "first.example:", proxy, "https", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := "http://backend.test/"
			if tt.tls {
				target = "https://backend.test/"
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.Host, req.RemoteAddr = tt.host, tt.peer
			if tt.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			req.Header.Set("X-Forwarded-Host", "different.example")
			if got := externalOrigin(req); got != tt.want {
				t.Errorf("external origin = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionBoundToProxyOrigin(t *testing.T) {
	a := &Auth{sessions: make(map[string]webSession)}
	r := gin.New()
	r.Use(a.guardOrigin)
	r.POST("/new", func(c *gin.Context) {
		if _, ok := a.newSession(c, "admin"); ok {
			c.Status(http.StatusNoContent)
		}
	})
	r.GET("/private", a.Require(), func(c *gin.Context) { c.Status(http.StatusNoContent) })

	request := func(method, path, host, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), method, "http://127.0.0.1"+path, nil)
		req.Host = host
		req.RemoteAddr = "192.0.2.10:12345"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Origin", origin)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if got := request("POST", "/new", "a.example:8443", "https://b.example", nil).Code; got != 403 {
		t.Fatalf("mismatched Origin returned %d", got)
	}
	login := request("POST", "/new", "a.example:8443", "https://a.example:8443", nil)
	if login.Code != 204 {
		t.Fatalf("proxied login returned %d", login.Code)
	}
	cookie := testCookie(t, login, "__Host-kimi_session")
	if !cookie.Secure || !cookie.HttpOnly || cookie.Domain != "" {
		t.Fatal("expected a host-only, secure session cookie")
	}
	for host, want := range map[string]int{"a.example:8443": 204, "a.example": 401, "b.example:8443": 401} {
		if got := request("GET", "/private", host, "", cookie).Code; got != want {
			t.Errorf("session on %s returned %d, want %d", host, got, want)
		}
	}
}
