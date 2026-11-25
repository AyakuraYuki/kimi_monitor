package web

import (
	"encoding/csv"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"kimi_balance_monitor/internal/config"
	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/store"
	"kimi_balance_monitor/internal/testdb"
)

func TestConsoleRoutes(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, testdb.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	hash, err := bcrypt.GenerateFromPassword([]byte("local-test-password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Web.Enabled = true
	cfg.Web.Username = "admin"
	cfg.Web.PasswordHash = string(hash)
	cfg.WeCom.WebhookURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test-fallback"
	cipher, err := secrets.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	router, err := newRouter(cfg, db, cipher)
	if err != nil {
		t.Fatal(err)
	}
	var session *http.Cookie
	var csrf string
	call := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := authRequest(t, router, method, path, body, testOrigin, csrf, session)
		if w.Code != want {
			t.Fatalf("%s %s returned %d, want %d", method, path, w.Code, want)
		}
		return w
	}
	if w := call("GET", "/", "", 200); !strings.Contains(w.Body.String(), "<html") {
		t.Fatal("missing embedded index")
	}
	call("GET", "/api/overview", "", 401)
	call("GET", "/api/consumption.csv", "", 401)
	w := call("POST", "/api/login", `{"username":"admin","password":"local-test-password"}`, 200)
	var auth struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &auth); err != nil {
		t.Fatal(err)
	}
	csrf, session = auth.Token, testCookie(t, w, "__Host-kimi_session")
	const key = "sk-abcdTESTconsole1234"
	const add = `{"api_key":"` + key + `","remark":"initial"}`
	call("POST", "/api/tokens", add, 201)
	call("POST", "/api/tokens", add, 409)
	call("PATCH", "/api/tokens/1", `{"remark":"updated","api_key":"replacement"}`, 400)
	call("PATCH", "/api/tokens/1", `{}`, 400)
	call("PATCH", "/api/tokens/1", `{"remark":"=signed, value"}`, 204)
	overview := call("GET", "/api/overview", "", 200)
	if strings.Contains(overview.Body.String(), key) || !strings.Contains(overview.Body.String(), `"status":"pending"`) {
		t.Fatal("unexpected pending token view")
	}
	now := time.Now().In(config.CST())
	// Both samples fall on the same Beijing date, including tests started at midnight.
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, config.CST())
	for _, available := range []int64{15_000_001, 12_000_000} {
		if _, err := db.RecordSuccess(ctx, 1, day, model.Balance{AvailableMicros: available}); err != nil {
			t.Fatal(err)
		}
	}
	overview = call("GET", "/api/overview", "", 200)
	if !strings.Contains(overview.Body.String(), `"today_consumption":"3.000001"`) {
		t.Fatal("overview lost precision or used a different accounting period")
	}
	date := now.Format(time.DateOnly)
	query := "?start=" + date + "&end=" + date
	daily := call("GET", "/api/consumption"+query, "", 200)
	if !strings.Contains(daily.Body.String(), `"consumption":"3.000001"`) {
		t.Fatal("missing daily consumption")
	}
	csvResponse := call("GET", "/api/consumption.csv"+query, "", 200)
	records, err := csv.NewReader(csvResponse.Body).ReadAll()
	if err != nil || len(records) != 2 ||
		!slices.Equal(records[0], []string{"日期", "令牌name", "令牌备注", "当天总计消耗"}) ||
		!slices.Equal(records[1], []string{date, "sk-abcd****1234", "'=signed, value", "3.000001"}) {
		t.Fatalf("unexpected CSV %q, err %v", records, err)
	}
	const settings = "/api/settings/webhook"
	checkSource := func(want string) {
		t.Helper()
		body := call("GET", settings, "", 200).Body.String()
		if strings.Contains(body, "key=") || !strings.Contains(body, `"source":"`+want+`"`) || !strings.Contains(body, `"has_fallback":true`) {
			t.Fatalf("unexpected settings response for %s", want)
		}
	}
	checkSource("config")
	call("PUT", settings, `{"url":"https://example.test/"}`, 400)
	call("PUT", settings, `{"url":"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test-custom"}`, 204)
	checkSource("override")
	call("DELETE", settings, "", 204)
	checkSource("config")
	call("DELETE", "/api/tokens/1", "", 204)
	call("DELETE", "/api/tokens/1", "", 404)
	if w := call("GET", "/api/consumption"+query, "", 200); !strings.Contains(w.Body.String(), `"rows":[]`) {
		t.Fatal("deleted token still appears in daily report")
	}
}

func TestReportRange(t *testing.T) {
	now := time.Date(2026, 4, 1, 1, 0, 0, 0, config.CST())
	for _, pair := range [][2]string{{"2026-03-01", "2026-02-28"}, {"2026-02-31", "2026-03-01"}, {"2020-02-01", "2026-01-01"}, {"2026-04-01", "2026-04-02"}} {
		if _, _, err := dateRange(pair[0], pair[1], now); err == nil {
			t.Errorf("accepted invalid range %q", pair)
		}
	}
	start, end, err := dateRange("2026-03-31", "2026-04-01", now)
	if err != nil || end.Sub(start) != 48*time.Hour || start.UTC().Hour() != 16 {
		t.Errorf("incorrect inclusive Beijing dates: %s, %s, %v", start, end, err)
	}
}
