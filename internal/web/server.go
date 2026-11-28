// Package web serves the embedded management console and its authenticated API.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"kimi_balance_monitor/internal/config"
	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/store"
)

//go:embed static/*
var assets embed.FS

type console struct {
	cfg    config.Config
	db     *store.Store
	cipher *secrets.Cipher
}

func newRouter(cfg config.Config, db *store.Store, cipher *secrets.Cipher) (*gin.Engine, error) {
	auth, err := NewAuth(cfg.Web)
	if err != nil {
		return nil, err
	}
	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		return nil, err
	}
	router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "服务暂时不可用"})
	}))
	auth.Register(router)

	files, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	for _, file := range entries {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		path := "/assets/" + name
		if name == "index.html" {
			path = "/"
			name = "/"
		}
		router.StaticFileFS(path, name, http.FS(files))
	}
	api := router.Group("/api", auth.Require(), func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	app := &console{cfg: cfg, db: db, cipher: cipher}
	api.GET("/overview", app.overview)
	api.POST("/tokens", app.addToken)
	api.PATCH("/tokens/:id", app.changeRemark)
	api.PUT("/tokens/:id/monitoring", app.changeMonitoring)
	api.DELETE("/tokens/:id", app.deleteToken)
	api.GET("/consumption", app.consumption)
	api.GET("/consumption.csv", app.consumption)
	api.GET("/settings/webhook", app.webhook)
	api.PUT("/settings/webhook", app.setWebhook)
	api.DELETE("/settings/webhook", app.clearWebhook)
	return router, nil
}

// Run shares the monitor's database and lifetime; public TLS is terminated by the reverse proxy.
func Run(ctx context.Context, cfg config.Config, db *store.Store, cipher *secrets.Cipher, logger *zerolog.Logger) error {
	gin.SetMode(gin.ReleaseMode)
	router, err := newRouter(cfg, db, cipher)
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Web.Listen)
	if err != nil {
		return fmt.Errorf("管理界面监听失败: %w", err)
	}
	server := &http.Server{
		Handler: router, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 75 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	done := make(chan struct{})
	stopShutdown := context.AfterFunc(ctx, func() {
		defer close(done)
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	})
	logger.Info().Str("listen", listener.Addr().String()).Msg("管理界面已启动")
	err = server.Serve(listener)
	if !stopShutdown() {
		<-done
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
