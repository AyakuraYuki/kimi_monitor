package monitor

import (
	"context"
	"sync"
	"testing"
	"time"

	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/store"
	"kimi_balance_monitor/internal/testdb"
)

type recordingAPI struct {
	mutex sync.Mutex
	calls map[string]int
}

func (a *recordingAPI) FetchBalance(ctx context.Context, token string) (model.Balance, error) {
	a.mutex.Lock()
	a.calls[token]++
	a.mutex.Unlock()
	return model.Balance{AvailableMicros: 50_000_000}, nil
}

func (a *recordingAPI) callCount(token string) int {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.calls[token]
}

func TestCheckAllSkipsPausedTokens(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	api := &recordingAPI{calls: make(map[string]int)}
	service, database := newIntegrationService(t, api)
	const plaintext = "sk-abcdefghijklmnop12345678"

	firstRound, err := service.CheckAll(ctx)
	if err != nil || len(firstRound) != 1 || api.callCount(plaintext) != 1 {
		t.Fatalf("首轮采集错误: results=%+v, calls=%d, err=%v", firstRound, api.callCount(plaintext), err)
	}
	before, err := database.LatestTokens(ctx)
	if err != nil || len(before) != 1 || before[0].CheckedAt == nil {
		t.Fatalf("读取首轮采集时间失败: %+v, %v", before, err)
	}
	if _, err := database.SetTokenMonitoring(ctx, "1", false); err != nil {
		t.Fatal(err)
	}
	pausedRound, err := service.CheckAll(ctx)
	if err != nil || len(pausedRound) != 0 || api.callCount(plaintext) != 1 {
		t.Fatalf("暂停后不应调用额度 API: results=%+v, calls=%d, err=%v", pausedRound, api.callCount(plaintext), err)
	}
	after, err := database.LatestTokens(ctx)
	if err != nil || len(after) != 1 || after[0].MonitoringEnabled || after[0].Error != "" ||
		after[0].CheckedAt == nil || !after[0].CheckedAt.Equal(*before[0].CheckedAt) {
		t.Fatalf("暂停不应生成失败记录: before=%+v, after=%+v, err=%v", before[0], after, err)
	}
	if _, err := database.SetTokenMonitoring(ctx, "1", true); err != nil {
		t.Fatal(err)
	}
	resumedRound, err := service.CheckAll(ctx)
	if err != nil || len(resumedRound) != 1 || api.callCount(plaintext) != 2 {
		t.Fatalf("恢复后应重新采集: results=%+v, calls=%d, err=%v", resumedRound, api.callCount(plaintext), err)
	}
}

func TestNewRoundReadsTokensFromIndependentManagementConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := testdb.DSN(t)
	runningStore, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer runningStore.Close()
	// 模拟独立 CLI 进程：单独连接同一个 PostgreSQL schema。
	managementStore, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer managementStore.Close()

	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i + 1)
	}
	cipher, err := secrets.NewCipher(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	addToken := func(plaintext string) model.Token {
		t.Helper()
		name, err := secrets.MaskToken(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		ciphertext, err := cipher.Encrypt(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		token, err := managementStore.AddToken(ctx, name, cipher.Digest(plaintext), ciphertext, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}

	const firstPlaintext = "sk-firsttoken1234567890"
	const secondPlaintext = "sk-secondtoken1234567890"
	first := addToken(firstPlaintext)
	api := &recordingAPI{calls: make(map[string]int)}
	service := &Service{Store: runningStore, API: api, Cipher: cipher, MaxParallel: 2}

	firstRound, err := service.CheckAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstRound) != 1 || firstRound[0].TokenID != first.ID {
		t.Fatalf("第一轮令牌列表错误: %+v", firstRound)
	}

	// 服务保持数据库连接打开，独立管理连接添加的令牌必须在下一轮出现。
	second := addToken(secondPlaintext)
	secondRound, err := service.CheckAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondRound) != 2 || secondRound[0].TokenID != first.ID || secondRound[1].TokenID != second.ID {
		t.Fatalf("第二轮未读取新令牌: %+v", secondRound)
	}
	if api.callCount(firstPlaintext) != 2 || api.callCount(secondPlaintext) != 1 {
		t.Fatalf("查询次数错误：first=%d second=%d", api.callCount(firstPlaintext), api.callCount(secondPlaintext))
	}

	if _, err := managementStore.RemoveToken(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	thirdRound, err := service.CheckAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(thirdRound) != 1 || thirdRound[0].TokenID != second.ID {
		t.Fatalf("第三轮未反映删除结果: %+v", thirdRound)
	}
	if api.callCount(firstPlaintext) != 2 || api.callCount(secondPlaintext) != 2 {
		t.Fatalf("删除后查询次数错误：first=%d second=%d", api.callCount(firstPlaintext), api.callCount(secondPlaintext))
	}
}
