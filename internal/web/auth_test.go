package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"

	"kimi_balance_monitor/internal/config"
)

const testOrigin = "https://console.example.test"

func authRequest(t *testing.T, router http.Handler, method, route, body, origin, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, testOrigin+route, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("X-CSRF-Token", csrf)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestOIDCDiscoveryRetriesAfterOutage(t *testing.T) {
	var calls atomic.Int32
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer idp.Close()
	a, err := NewAuth(config.WebConfig{
		Listen: "127.0.0.1:8080",
		OIDC:   config.OIDCConfig{Issuer: idp.URL, ClientID: "monitor", ClientSecret: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.oidc.http = idp.Client()
	r := gin.New()
	a.Register(r)
	for range 2 {
		res := authRequest(t, r, "GET", "/auth/oidc/start", "", "", "", nil)
		if res.Header().Get("Location") != "/?login_error=oidc" {
			t.Fatal("missing SSO outage response")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("repeated discovery during outage backoff")
	}
	a.oidc.retryAt = time.Now().Add(-time.Second)
	authRequest(t, r, "GET", "/auth/oidc/start", "", "", "", nil)
	if calls.Load() != 2 {
		t.Fatal("discovery did not recover automatically")
	}
}

func testCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response did not set cookie %q", name)
	return nil
}

func TestLocalSessionLifecycle(t *testing.T) {
	pass := "sample-admin-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.WebConfig{Listen: "127.0.0.1:8080", Username: "admin", PasswordHash: string(hash)}
	a, err := NewAuth(cfg)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	a.Register(router)
	router.GET("/api/test", a.Require(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	login := `{"username":"admin","password":"sample-admin-password"}`

	for _, test := range []struct {
		origin, body string
		want         int
	}{
		{"https://other.test", login, 403}, {"", login, 403},
		{testOrigin, `{"username":"admin","password":"bad"}`, 401},
		{testOrigin, `{"extra":true}`, 400}, {testOrigin, `{"username":"a","username":"b"}`, 400},
		{testOrigin, `{"username":"a"}{}`, 400},
	} {
		w := authRequest(t, router, "POST", "/api/login", test.body, test.origin, "", nil)
		if w.Code != test.want {
			t.Errorf("login returned %d, want %d", w.Code, test.want)
		}
	}
	w := authRequest(t, router, "POST", "/api/login", login, testOrigin, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login returned %d", w.Code)
	}
	cookie := testCookie(t, w, "__Host-kimi_session")
	if !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("session cookie flags")
	}
	var payload struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.Token == "" {
		t.Fatal("missing CSRF token")
	}
	if got := authRequest(t, router, "GET", "/api/test", "", "", "", cookie).Code; got != 204 {
		t.Fatalf("protected read = %d", got)
	}
	if got := authRequest(t, router, "POST", "/api/logout", "", testOrigin, "", cookie).Code; got != 403 {
		t.Fatalf("unchecked logout = %d", got)
	}
	if got := authRequest(t, router, "POST", "/api/logout", "", testOrigin, payload.Token, cookie).Code; got != 204 {
		t.Fatalf("logout = %d", got)
	}
	if got := authRequest(t, router, "GET", "/api/test", "", "", "", cookie).Code; got != 401 {
		t.Fatalf("expired read = %d", got)
	}
	// Only the incorrect password used a slot. At the last slot a valid sign-in
	// should still be accepted, including a second user at the same proxy.
	for range 8 {
		w = authRequest(t, router, "POST", "/api/login", `{"username":"admin","password":"wrong"}`, testOrigin, "", nil)
	}
	for range 2 {
		if got := authRequest(t, router, "POST", "/api/login", login, testOrigin, "", nil).Code; got != 200 {
			t.Fatalf("valid sign-in counted against shared failure budget: %d", got)
		}
	}
	for range 2 {
		w = authRequest(t, router, "POST", "/api/login", `{"username":"admin","password":"wrong"}`, testOrigin, "", nil)
	}
	if w.Code != 429 {
		t.Errorf("repeated logins = %d", w.Code)
	}
}

func TestOIDCAuthorizationFlow(t *testing.T) {
	// Exercise discovery, token exchange, JWKS and the signed ID token over TLS.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: key, KeyID: "test", Algorithm: "ES256", Use: "sig"}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jwk}, nil)
	if err != nil {
		t.Fatal(err)
	}
	type exchange struct {
		challenge, nonce, redirectURI string
	}
	var authorization exchange // assigned before the callback and read by the token endpoint
	mux := http.NewServeMux()
	idp := httptest.NewTLSServer(mux)
	defer idp.Close()
	issuer := idp.URL + "/application/o/monitor/"
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, value); err != nil {
			t.Error(err)
		}
	}
	mux.HandleFunc("GET /application/o/monitor/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, gin.H{"issuer": issuer, "authorization_endpoint": idp.URL + "/authorize", "token_endpoint": idp.URL + "/token", "jwks_uri": idp.URL + "/keys", "id_token_signing_alg_values_supported": []string{"ES256"}})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk.Public()}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		code := r.FormValue("code")
		client, secret, ok := r.BasicAuth()
		if !ok || client != "monitor" || secret != "secret" || r.FormValue("redirect_uri") != authorization.redirectURI || oauth2.S256ChallengeFromVerifier(r.FormValue("code_verifier")) != authorization.challenge {
			t.Error("unexpected token exchange parameters")
			http.Error(w, "invalid client", http.StatusUnauthorized)
			return
		}
		// Application access is granted by the IdP. An ID token needs no groups claim.
		claims := gin.H{"iss": issuer, "aud": client, "exp": time.Now().Add(time.Minute).Unix(), "sub": "subject", "preferred_username": "operator", "nonce": authorization.nonce}
		switch code {
		case "wrong-nonce":
			claims["nonce"] = "wrong"
		case "wrong-audience":
			claims["aud"] = "other"
		}
		raw, err := json.Marshal(claims)
		if err != nil {
			t.Error(err)
			return
		}
		signed, err := signer.Sign(raw)
		if err != nil {
			t.Error(err)
			return
		}
		serialized, err := signed.CompactSerialize()
		if err != nil {
			t.Error(err)
			return
		}
		writeJSON(w, gin.H{"access_token": "test-token", "token_type": "Bearer", "id_token": serialized})
	})

	for _, code := range []string{"wrong-nonce", "wrong-audience", "valid-without-groups", "valid-via-proxy"} {
		t.Run(code, func(t *testing.T) {
			cfg := config.WebConfig{
				Listen: "127.0.0.1:8080",
				OIDC:   config.OIDCConfig{Issuer: issuer, ClientID: "monitor", ClientSecret: "secret"},
			}
			proxied := code == "valid-via-proxy"
			origin := "https://monitor.example.test:9443"
			if proxied {
				origin = "https://console.example.test"
			}
			a, err := NewAuth(cfg)
			if err != nil {
				t.Fatal(err)
			}
			a.oidc.http = idp.Client()
			router := gin.New()
			a.Register(router)
			get := func(origin, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
				target := origin + path
				if proxied {
					target = "http" + strings.TrimPrefix(target, "https")
				}
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
				if proxied {
					req.RemoteAddr = "127.0.0.1:3456"
					req.Header.Set("X-Forwarded-Proto", "https")
				}
				if cookie != nil {
					req.AddCookie(cookie)
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				return w
			}
			start := get(origin, "/auth/oidc/start", nil)
			u, err := url.Parse(start.Header().Get("Location"))
			if err != nil || u.Path != "/authorize" {
				t.Fatalf("unexpected authorization redirect %v", u)
			}
			fields := u.Query()
			authorization = exchange{challenge: fields.Get("code_challenge"), nonce: fields.Get("nonce"), redirectURI: origin + "/auth/oidc/callback"}
			if fields.Get("redirect_uri") != authorization.redirectURI {
				t.Fatal("callback does not match initiating origin")
			}
			if fields.Get("state") == "" || authorization.nonce == "" || authorization.challenge == "" || fields.Get("code_challenge_method") != "S256" {
				t.Fatal("missing state, nonce or PKCE challenge")
			}
			flow := testCookie(t, start, "__Host-kimi_session_oidc")
			path := "/auth/oidc/callback?code=" + code + "&state=" + fields.Get("state")
			unbound := get(origin, path, nil)
			if unbound.Header().Get("Location") != "/?login_error=oidc" {
				t.Fatal("callback without browser binding")
			}
			// Cookies are scoped by host, so the same host on another HTTPS port
			// can receive a cookie. It must not be able to consume this flow.
			crossOrigin := get("https://monitor.example.test:8443", path, flow)
			if crossOrigin.Header().Get("Location") != "/?login_error=oidc" {
				t.Fatal("callback consumed on a different origin")
			}
			res := get(origin, path, flow)
			if code == "valid-without-groups" || proxied {
				if res.Header().Get("Location") != "/" {
					t.Fatal("valid callback denied")
				}
				sess := testCookie(t, res, "__Host-kimi_session")
				if got := get(origin, "/api/session", sess); !strings.Contains(got.Body.String(), `"username":"operator"`) {
					t.Fatalf("identity missing: %s", got.Body.String())
				}
			} else if res.Header().Get("Location") != "/?login_error=oidc" {
				t.Fatal("unapproved identity accepted")
			}
			replay := get(origin, path, flow)
			if replay.Header().Get("Location") != "/?login_error=oidc" {
				t.Fatal("callback was not consumed")
			}
		})
	}
}
