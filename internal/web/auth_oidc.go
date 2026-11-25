package web

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
)

const oidcLifetime = 5 * time.Minute

type oidcLogin struct {
	origin   string
	state    string
	nonce    string
	verifier string
	expires  time.Time
	slot     loginReservation
}

type oidcClient struct {
	http     *http.Client
	mu       sync.Mutex
	provider *oidc.Provider
	retryAt  time.Time
}

func newOIDCClient() oidcClient {
	return oidcClient{http: &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (a *Auth) oidcProvider(ctx context.Context) (*oidc.Provider, error) {
	a.oidc.mu.Lock()
	defer a.oidc.mu.Unlock()
	if a.oidc.provider != nil {
		return a.oidc.provider, nil
	}
	// Concurrent starts must not each repeat a slow discovery after an IdP outage.
	if time.Now().Before(a.oidc.retryAt) {
		return nil, errors.New("OIDC 服务暂时不可用")
	}
	provider, err := oidc.NewProvider(ctx, a.cfg.OIDC.Issuer)
	if err != nil {
		if ctx.Err() == nil {
			a.oidc.retryAt = time.Now().Add(10 * time.Second)
		}
		return nil, err
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, err
	}
	for _, endpoint := range []string{provider.Endpoint().AuthURL, provider.Endpoint().TokenURL, metadata.JWKS} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return nil, errors.New("OIDC 服务端点必须使用 HTTPS")
		}
	}
	a.oidc.provider = provider
	return provider, nil
}

func (a *Auth) oauthConfig(provider *oidc.Provider, origin string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     a.cfg.OIDC.ClientID,
		ClientSecret: a.cfg.OIDC.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  origin + "/auth/oidc/callback",
		Scopes:       []string{oidc.ScopeOpenID, "profile"},
	}
}

func (a *Auth) startOIDC(c *gin.Context) {
	if a.cfg.OIDC.Issuer == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	slot, ok := a.reserveLogin(c, "oidc")
	if !ok {
		return
	}
	provider, err := a.oidcProvider(oidc.ClientContext(c.Request.Context(), a.oidc.http))
	if err != nil {
		c.Redirect(http.StatusSeeOther, "/?login_error=oidc")
		return
	}
	now := time.Now()
	flow := oidcLogin{
		origin:   requestOrigin(c),
		state:    rand.Text(),
		nonce:    rand.Text(),
		verifier: oauth2.GenerateVerifier(),
		expires:  now.Add(oidcLifetime),
		slot:     slot,
	}
	id := rand.Text()
	cookie := sessionCookieName(c) + "_oidc"
	a.mu.Lock()
	maps.DeleteFunc(a.pending, func(_ string, f oidcLogin) bool { return !f.expires.After(now) })
	old, _ := c.Cookie(cookie)
	if a.pending[old].origin == flow.origin {
		delete(a.pending, old)
	}
	if len(a.pending) >= 1024 {
		a.mu.Unlock()
		c.Redirect(http.StatusSeeOther, "/?login_error=oidc")
		return
	}
	a.pending[id] = flow
	a.mu.Unlock()
	a.setCookie(c, cookie, id, int(oidcLifetime.Seconds()))
	c.Redirect(http.StatusSeeOther, a.oauthConfig(provider, flow.origin).AuthCodeURL(
		flow.state,
		oidc.Nonce(flow.nonce),
		oauth2.S256ChallengeOption(flow.verifier),
	))
}

func (a *Auth) finishOIDC(c *gin.Context) {
	c.Header("Referrer-Policy", "no-referrer")
	cookie := sessionCookieName(c) + "_oidc"
	id, _ := c.Cookie(cookie)
	a.mu.Lock()
	flow, ok := a.pending[id]
	if ok && flow.origin == requestOrigin(c) && subtle.ConstantTimeCompare([]byte(c.Query("state")), []byte(flow.state)) == 1 {
		// Consume once before exchanging the code, even if the provider rejects it.
		delete(a.pending, id)
	} else {
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		c.Redirect(http.StatusSeeOther, "/?login_error=oidc")
		return
	}
	a.setCookie(c, cookie, "", -1)
	code := c.Query("code")
	if !flow.expires.After(time.Now()) || code == "" || c.Query("error") != "" {
		c.Redirect(http.StatusSeeOther, "/?login_error=oidc")
		return
	}
	ctx := oidc.ClientContext(c.Request.Context(), a.oidc.http)
	username, err := a.verifyOIDC(ctx, code, flow)
	if err != nil {
		// Provider errors and callback query strings can contain credentials.
		c.Redirect(http.StatusSeeOther, "/?login_error=oidc")
		return
	}
	if _, ok := a.newSession(c, username); ok {
		a.refundLogin(flow.slot)
		c.Redirect(http.StatusSeeOther, "/")
	}
}

func (a *Auth) verifyOIDC(ctx context.Context, code string, flow oidcLogin) (string, error) {
	provider, err := a.oidcProvider(ctx)
	if err != nil {
		return "", err
	}
	token, err := a.oauthConfig(provider, flow.origin).Exchange(ctx, code, oauth2.VerifierOption(flow.verifier))
	if err != nil {
		return "", err
	}
	raw, _ := token.Extra("id_token").(string)
	id, err := provider.Verifier(&oidc.Config{ClientID: a.cfg.OIDC.ClientID}).Verify(ctx, raw)
	if err != nil {
		return "", err
	}
	if id.Subject == "" || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(flow.nonce)) != 1 {
		return "", errors.New("OIDC 身份校验失败")
	}
	var claims struct {
		Username string `json:"preferred_username"`
		Party    string `json:"azp"`
	}
	if err := id.Claims(&claims); err != nil {
		return "", err
	}
	if (len(id.Audience) > 1 || claims.Party != "") && claims.Party != a.cfg.OIDC.ClientID {
		return "", errors.New("OIDC 客户端校验失败")
	}
	return cmp.Or(claims.Username, id.Subject), nil
}
