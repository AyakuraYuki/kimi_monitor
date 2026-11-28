package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"kimi_balance_monitor/internal/config"
	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/store"
	"kimi_balance_monitor/internal/wecom"
)

func amount(micros *int64) *string {
	if micros == nil {
		return nil
	}
	return new(model.DecimalCNY(*micros))
}

type tokenView struct {
	ID                int64      `json:"id"`
	Name              string     `json:"name"`
	Remark            string     `json:"remark"`
	CreatedAt         time.Time  `json:"created_at"`
	MonitoringEnabled bool       `json:"monitoring_enabled"`
	CheckedAt         *time.Time `json:"checked_at"`
	BalanceAt         *time.Time `json:"balance_at"`
	Available         *string    `json:"available"`
	Cash              *string    `json:"cash"`
	Voucher           *string    `json:"voucher"`
	TodayConsumption  *string    `json:"today_consumption"`
	Status            string     `json:"status"`
	Error             string     `json:"error,omitempty"`
}

func (app *console) overview(c *gin.Context) {
	ctx := c.Request.Context()
	now := time.Now().In(config.CST())
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	data, err := app.db.Overview(ctx, start, now)
	if err != nil {
		storeError(c, err)
		return
	}
	var notification gin.H
	if last := data.Notification; last != nil {
		notification = gin.H{"text": last.Text, "sent_at": last.SentAt, "delivered": last.Delivered, "total": last.Total}
	}
	result := make([]tokenView, 0, len(data.Tokens))
	for _, item := range data.Tokens {
		row := tokenView{
			ID:                item.ID,
			Name:              item.Name,
			Remark:            item.Remark,
			CreatedAt:         item.CreatedAt,
			MonitoringEnabled: item.MonitoringEnabled,
			CheckedAt:         item.CheckedAt,
			BalanceAt:         item.BalanceAt,
			Error:             item.Error,
			Available:         amount(item.AvailableMicros),
			Cash:              amount(item.CashMicros),
			Voucher:           amount(item.VoucherMicros),
			Status:            "pending",
		}
		if !item.MonitoringEnabled {
			row.Status = "paused"
		} else if item.Error != "" {
			row.Status = string(model.HealthError)
		} else if item.AvailableMicros != nil {
			row.Status = string(model.BalanceHealth(*item.AvailableMicros, app.cfg.LowBalanceMicros()))
		}
		if item.BalanceAt != nil && !item.BalanceAt.Before(start) {
			row.TodayConsumption = new(model.DecimalCNY(data.Consumption[item.ID]))
		}
		result = append(result, row)
	}
	c.JSON(http.StatusOK, gin.H{
		"tokens": result, "notification": notification, "threshold": model.DecimalCNY(app.cfg.LowBalanceMicros()),
		"interval_seconds": app.cfg.Monitor.Interval.Seconds(), "today": now.Format(time.DateOnly),
	})
}

func validRemark(c *gin.Context, remark string) bool {
	if utf8.RuneCountInString(remark) > 256 || strings.ContainsRune(remark, 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "备注最多支持 256 个字符，不能含空字符"})
		return false
	}
	return true
}

func (app *console) addToken(c *gin.Context) {
	var form struct {
		Key    string `json:"api_key"`
		Remark string `json:"remark"`
	}
	if !decodeJSON(c, &form) || !validRemark(c, form.Remark) {
		return
	}
	key := strings.TrimSpace(form.Key)
	if len(key) > 4096 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "令牌长度超过 4096 字节"})
		return
	}
	name, err := secrets.MaskToken(key)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	encrypted, err := app.cipher.Encrypt(key)
	if err != nil {
		storeError(c, err)
		return
	}
	token, err := app.db.AddTokenWithRemark(c.Request.Context(), name, strings.TrimSpace(form.Remark), app.cipher.Digest(key), encrypted, time.Now())
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": token.ID, "name": token.Name})
}

func tokenID(c *gin.Context) (string, bool) {
	selector := c.Param("id")
	if id, err := strconv.ParseInt(selector, 10, 64); err != nil || id < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "令牌 ID 必须是正整数"})
		return "", false
	}
	return selector, true
}

func (app *console) changeRemark(c *gin.Context) {
	id, ok := tokenID(c)
	if !ok {
		return
	}
	var form struct {
		Remark *string `json:"remark"`
	}
	if !decodeJSON(c, &form) {
		return
	}
	if form.Remark == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供备注，空字符串可清除备注"})
		return
	}
	if !validRemark(c, *form.Remark) {
		return
	}
	if _, err := app.db.SetTokenRemark(c.Request.Context(), id, *form.Remark); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (app *console) changeMonitoring(c *gin.Context) {
	id, ok := tokenID(c)
	if !ok {
		return
	}
	var form struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(c, &form) {
		return
	}
	if form.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供 enabled 布尔值"})
		return
	}
	if _, err := app.db.SetTokenMonitoring(c.Request.Context(), id, *form.Enabled); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (app *console) deleteToken(c *gin.Context) {
	id, ok := tokenID(c)
	if !ok {
		return
	}
	if _, err := app.db.RemoveToken(c.Request.Context(), id); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (app *console) webhook(c *gin.Context) {
	stored, err := app.db.WebhookOverride(c.Request.Context())
	if err != nil {
		storeError(c, err)
		return
	}
	fallback := strings.TrimSpace(app.cfg.WeCom.WebhookURL) != ""
	source := "none"
	if len(stored) > 0 {
		source = "override"
	} else if fallback {
		source = "config"
	}
	c.JSON(http.StatusOK, gin.H{"source": source, "has_fallback": fallback})
}

func (app *console) setWebhook(c *gin.Context) {
	var form struct {
		URL string `json:"url"`
	}
	if !decodeJSON(c, &form) {
		return
	}
	raw := strings.TrimSpace(form.URL)
	if err := wecom.ValidateManagedURL(raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ciphertext, err := app.cipher.EncryptWebhook(raw)
	if err != nil {
		storeError(c, err)
		return
	}
	app.saveWebhook(c, ciphertext)
}

func (app *console) clearWebhook(c *gin.Context) {
	app.saveWebhook(c, nil)
}

func (app *console) saveWebhook(c *gin.Context, ciphertext []byte) {
	if err := app.db.SetWebhookOverride(c.Request.Context(), ciphertext); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func storeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrDuplicateToken):
		c.JSON(http.StatusConflict, gin.H{"error": store.ErrDuplicateToken.Error()})
	case errors.Is(err, store.ErrTokenNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": store.ErrTokenNotFound.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "数据访问失败，请稍后重试"})
	}
}
