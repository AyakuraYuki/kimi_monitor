package web

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"kimi_balance_monitor/internal/config"
	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/store"
)

// dateRange converts inclusive Beijing calendar dates to a half-open database interval.
func dateRange(startDate, endDate string, now time.Time) (time.Time, time.Time, error) {
	loc := config.CST()
	start, startErr := time.ParseInLocation(time.DateOnly, startDate, loc)
	end, endErr := time.ParseInLocation(time.DateOnly, endDate, loc)
	if startErr != nil || endErr != nil || start.Year() == 0 || end.Year() == 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("请提供 YYYY-MM-DD 格式的开始和结束日期")
	}
	if end.Before(start) || end.After(now.In(loc)) || end.After(start.AddDate(0, 0, 365)) {
		return time.Time{}, time.Time{}, fmt.Errorf("请选择截至今天且按先后顺序排列的日期，单次最多查询 366 天")
	}
	return start, end.AddDate(0, 0, 1), nil
}

func (app *console) consumption(c *gin.Context) {
	start, end, err := dateRange(c.Query("start"), c.Query("end"), time.Now())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rows, err := app.db.DailyConsumption(c.Request.Context(), start, end)
	if err != nil {
		storeError(c, err)
		return
	}
	if strings.HasSuffix(c.FullPath(), ".csv") {
		filename := fmt.Sprintf("kimi-consumption_%s_%s.csv", c.Query("start"), c.Query("end"))
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.Header("Content-Type", "text/csv; charset=utf-8")
		writeConsumptionCSV(csv.NewWriter(c.Writer), rows)
		return
	}
	data := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		data = append(data, gin.H{
			"date": row.Date, "token_id": row.TokenID, "name": row.Name, "remark": row.Remark,
			"consumption": amount(row.ConsumptionMicros),
		})
	}
	c.JSON(http.StatusOK, gin.H{"source": "balance_snapshots", "rows": data})
}

func writeConsumptionCSV(w *csv.Writer, rows []store.DailyRow) {
	w.UseCRLF = true
	_ = w.Write([]string{"日期", "令牌name", "令牌备注", "当天总计消耗"})
	for _, row := range rows {
		value := ""
		if row.ConsumptionMicros != nil {
			value = model.DecimalCNY(*row.ConsumptionMicros)
		}
		if err := w.Write([]string{row.Date, csvText(row.Name), csvText(row.Remark), value}); err != nil {
			return
		}
	}
	w.Flush()
}

// Protect spreadsheet users when a user-supplied text cell starts like a formula.
func csvText(s string) string {
	trimmed := strings.TrimLeftFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' })
	first, _ := utf8.DecodeRuneInString(trimmed)
	if strings.HasPrefix(s, "\t") || strings.HasPrefix(s, "\r") || strings.HasPrefix(s, "\n") ||
		strings.ContainsRune("=+-@＝＋－＠", first) {
		return "'" + s
	}
	return s
}
