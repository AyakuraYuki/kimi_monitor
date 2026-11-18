package monitor

import (
	"context"
	"sync"
	"testing"
	"time"

	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/secrets"
	"kimi_balance_monitor/internal/store"
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

func TestNewRoundReadsTokensFromIndependentManagementConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	databasePath := t.TempDir() + "/shared.db"
	runningStore, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer runningStore.Close()
	// 模拟独立 CLI 进程：单独打开同一个 SQLite 数据库。
	managementStore, err := store.Open(ctx, databasePath)
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
