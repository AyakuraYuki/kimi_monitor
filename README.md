# Kimi 令牌余额监控

一个使用 Go 编写的定时监控服务：通过 Kimi 开放平台接口查询所有令牌的余额，将加密令牌和余额历史保存到 SQLite，并通过企业微信群机器人发送 Markdown 整体报告。

## 功能

- 命令行添加、查看、设置备注、删除令牌。
- 令牌使用 AES-256-GCM 加密后保存；去重使用独立密钥派生的 HMAC-SHA256 摘要。
- 数据库唯一约束阻止重复添加，管理名统一为 `sk-abcd****0123`。
- 启动后立即查询，之后默认每 10 分钟进行一轮全量查询。
- 每次成功查询都保存可用、现金、代金券余额；失败查询单独保存错误，余额字段为 `NULL`。
- 金额以整数微元保存和比较，不使用浮点数计算环比。
- 区分余额状态与余额变动，严格识别低余额、补充、消耗、耗尽和恢复可用。
- 企业微信使用 `markdown` 消息类型；超过 4096 字节时按完整令牌块自动分批。
- 支持单次执行、并发查询、优雅退出和查询失败后的历史衔接。

## 构建

需要 Go 1.27 或更新版本。数据库操作使用 GORM，SQLite 使用纯 Go 驱动，不依赖 CGO。

```bash
go build -o bin/kimi-monitor ./cmd/kimi-monitor
```

## YAML 配置

程序不读取环境变量作为服务配置，默认加载当前目录的 `config.yaml`。可通过全局参数指定其他路径：

```bash
./bin/kimi-monitor --config /path/to/config.yaml list
```

先复制示例并生成主密钥：

```bash
cp config.example.yaml config.yaml
./bin/kimi-monitor generate-key
```

将生成的密钥填入 `security.master_key`，同时填写企业微信 webhook，然后限制配置文件权限：

```bash
chmod 600 config.yaml
```

完整配置示例：

```yaml
database:
  # 相对路径以配置文件所在目录为基准，而不是进程工作目录。
  path: "kimi_monitor.db"

security:
  # 使用 generate-key 生成，不是 Kimi API Key。
  master_key: "替换为32字节主密钥的Base64或十六进制表示"

kimi:
  api_base_url: "https://api.moonshot.cn"

monitor:
  interval: "10m"
  low_balance: 10
  concurrency: 4

wecom:
  webhook_url: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=替换为机器人密钥"
```

| YAML 配置项 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `security.master_key` | 添加和运行时必填 | 无 | 32 字节主密钥的 Base64 或十六进制表示 |
| `database.path` | 否 | `kimi_monitor.db` | SQLite 路径；相对路径锚定到配置文件目录 |
| `monitor.low_balance` | 否 | `10` | 低余额预警线，单位人民币元 |
| `monitor.interval` | 否 | `10m` | 查询间隔，使用 Go duration 格式 |
| `monitor.concurrency` | 否 | `4` | 单轮并发查询数 |
| `kimi.api_base_url` | 否 | `https://api.moonshot.cn` | 与 API Key 所属平台一致的 API 源站 |
| `wecom.webhook_url` | 运行时必填 | 无 | 企业微信群机器人完整 webhook 地址 |

YAML 解析会拒绝未知字段。配置文件可能同时包含主密钥和 webhook 密钥，因此权限必须设为 `0600`，否则命令会拒绝加载。

**主密钥不会写入数据库。必须安全、持久地保存它；丢失后已有令牌无法解密，需要删除并重新添加。不要在同一个数据库上随意更换主密钥。**

国内站和国际站的 API Key 独立。使用非默认站点时，请将 `kimi.api_base_url` 设置为该站点的 API 源站；程序会请求其 `/v1/users/me/balance` 路径。

## 令牌管理

推荐使用标准输入添加，避免令牌进入 shell 历史和进程参数：

```bash
printf '%s\n' 'sk-实际令牌' | ./bin/kimi-monitor add --stdin
# 输出包含 token_id 和打码后 token_name 字段的 JSON 状态日志
```

也支持直接参数，但不推荐：

```bash
./bin/kimi-monitor add 'sk-实际令牌'
```

查看、设置备注和删除：

```bash
./bin/kimi-monitor list
./bin/kimi-monitor remark 1 '生产环境主账号'
./bin/kimi-monitor remark 'sk-abcd****5678' '备用账号'
./bin/kimi-monitor remark 1 ''  # 清除备注
./bin/kimi-monitor remove 1
./bin/kimi-monitor remove 'sk-abcd****5678'
```

备注保存在 `tokens.remark` 列；`list` 和企业微信报告都会展示非空备注。打开旧版本数据库时，GORM 会自动执行幂等迁移并补充该列，无需手工修改表结构。

不同令牌可能出现相同的首尾打码名称，因此名称碰撞时仍允许添加，但按名称设置备注或删除会要求改用数字 ID。删除令牌会级联删除其余额历史。

## 运行

持续运行，启动时立即执行一轮，之后每 10 分钟执行一轮：

```bash
./bin/kimi-monitor run
```

只执行一轮，适用于调试或外部定时器：

```bash
./bin/kimi-monitor run --once
```

`run` 仅有一个可选参数：

```text
--once    只执行一轮
```

查询间隔、预警线和并发数均在 YAML 的 `monitor` 节点中配置。没有配置令牌时跳过查询和推送。单个令牌失败不会中断其他令牌，错误会写入数据库并体现在整体报告中；`run --once` 只在存储或推送失败等整体错误时返回非零退出码。

### 日志格式

状态与运行日志使用 zerolog 输出为单行 JSON，并包含 `level`、`time`、`message` 字段。令牌数量、查询异常数、消息数和打码令牌名等数据使用独立的结构化字段，不再拼接到消息文本中。例如，单轮完成日志包含：

```json
{"level":"info","token_count":2,"failure_count":0,"message_count":1,"time":"2026-11-18T01:00:00+08:00","message":"本轮完成"}
```

- 整个进程只在入口处创建一个 logger，CLI 与持续监控服务共同使用。
- 所有状态、错误和持续运行日志统一写入标准输出，不再主动写入标准错误；Supervisor 配置仍会合并标准错误，以捕获 Go 运行时等异常输出。
- `help`、`generate-key` 的密钥以及 `list` 的表格属于功能性输出，不使用日志格式。

## 服务运行期间管理令牌

服务与独立 CLI 命令必须使用**同一份 YAML 配置**：

```bash
# 终端 1：持续运行
./bin/kimi-monitor --config /path/to/config.yaml run

# 终端 2：服务不停止，直接添加或删除
printf '%s\n' 'sk-新令牌' | ./bin/kimi-monitor --config /path/to/config.yaml add --stdin
./bin/kimi-monitor --config /path/to/config.yaml remove 2
```

服务每一轮都会重新读取 SQLite 的 `tokens` 表，不在内存中缓存令牌清单。独立 CLI 通过同一数据库写入后：

- 新增令牌最迟在下一轮自动加入查询和推送，无需重启服务。
- 删除令牌最迟在下一轮停止查询。
- 如果管理操作发生在某一轮查询进行中，该轮可能沿用轮次开始时的清单，下一轮会读取最新清单。
- SQLite 使用 WAL 模式和 5 秒 busy timeout，支持服务与短时管理命令同时访问。

YAML 配置本身的修改不会自动热加载，修改后需要重启服务；令牌增删不需要重启。

## 状态与环比规则

**状态和变动是两个独立维度。** 状态描述现在能否使用，变动描述与最近一次成功快照相比发生了什么。

状态按以下优先级计算：

| 条件 | 状态 |
| --- | --- |
| 解密、查询或保存失败 | `⚠️ 查询异常` |
| `available_balance <= 0` | `🔴 已用尽` |
| `0 < available_balance <= 预警线` | `🟡 余额不足` |
| `available_balance > 预警线` | `🟢 正常` |

变动仅使用 `available_balance`：

| 条件 | 变动标识 |
| --- | --- |
| 没有历史成功快照 | `🆕 首次记录` |
| 当前值 < 上次成功值 | `📉 消耗` |
| 当前值 > 上次成功值 | `📈 补充` |
| 当前值 = 上次成功值 | `➖ 无变化` |

组合情况的严格处理：

- 从非正余额补充到正余额：状态恢复正常或低余额，同时显示“已恢复可用”。
- 余额增加但仍小于等于 0：仍为“已用尽”，同时显示“已补充，但仍不可用”。
- 余额增加但仍低于预警线：仍为“余额不足”，同时显示补充金额。
- 本轮消耗后变为非正余额：显示“已用尽”和“本轮耗尽”。
- 查询失败：不生成余额快照，不计算消耗或补充；恢复后与**最近一次成功**快照比较，而不是与失败记录或 0 比较。

## 企业微信报告示例

实际消息使用企业微信支持的标题、引用、加粗、行内代码和 `<font>` 颜色标记，不使用兼容性较差的表格排版。

```markdown
## Kimi 令牌余额监控
> 检查时间：2026-11-18 01:00:00 CST
> 令牌：2 个 ｜ 🟢 正常 1 ｜ 🟡 预警 1 ｜ 🔴 用尽 0 ｜ ⚠️ 异常 0
> 可用余额合计：**¥54.588940** ｜ 本轮补充：0 个
> 低余额预警线：¥10.000000

### #1 `sk-abcd****5678`
> 备注：生产环境主账号
<font color="info">🟢 正常</font>　📉 消耗 **¥0.100000**
> 可用余额：**¥49.588940**
> 上次可用：¥49.688940（2026-11-18 00:50:00 CST）
> 现金：¥3.000010 ｜ 代金券：¥46.588930

### #2 `sk-wxyz****9012`
<font color="warning">🟡 余额不足</font>　可用余额：**¥5.000000**
```

## Supervisor 部署

配置模板位于 `deploy/supervisor/kimi-monitor.conf`，默认使用以下路径：

| 用途 | 路径 |
| --- | --- |
| 二进制 | `/opt/kimi-monitor/bin/kimi-monitor` |
| YAML 配置 | `/etc/kimi-monitor/config.yaml` |
| SQLite 数据 | `/var/lib/kimi-monitor/kimi_monitor.db` |
| 运行日志 | `/var/log/kimi-monitor/monitor.log` |

### 创建用户和目录

以下命令适用于常见 Linux 发行版，nologin 路径可按系统调整：

```bash
sudo useradd --system --home-dir /opt/kimi-monitor \
  --shell /usr/sbin/nologin kimi-monitor

sudo install -d -o root -g root -m 0755 /opt/kimi-monitor/bin
sudo install -o root -g root -m 0755 bin/kimi-monitor \
  /opt/kimi-monitor/bin/kimi-monitor

sudo install -d -o root -g kimi-monitor -m 0750 /etc/kimi-monitor
sudo install -d -o kimi-monitor -g kimi-monitor -m 0700 /var/lib/kimi-monitor
sudo install -d -o kimi-monitor -g kimi-monitor -m 0750 /var/log/kimi-monitor

sudo install -o kimi-monitor -g kimi-monitor -m 0600 \
  config.example.yaml /etc/kimi-monitor/config.yaml
```

编辑 `/etc/kimi-monitor/config.yaml`，至少修改：

```yaml
database:
  path: "/var/lib/kimi-monitor/kimi_monitor.db"
security:
  master_key: "使用 generate-key 生成的主密钥"
wecom:
  webhook_url: "完整企业微信机器人 webhook"
```

主密钥可直接在服务器上生成：

```bash
/opt/kimi-monitor/bin/kimi-monitor generate-key
sudo chmod 600 /etc/kimi-monitor/config.yaml
```

### 安装并启动 Supervisor 配置

Ubuntu/Debian 的常见安装路径：

```bash
sudo install -o root -g root -m 0644 \
  deploy/supervisor/kimi-monitor.conf \
  /etc/supervisor/conf.d/kimi-monitor.conf

sudo supervisorctl reread
sudo supervisorctl update
sudo supervisorctl status kimi-monitor
```

RHEL 系发行版通常使用 `/etc/supervisord.d/kimi-monitor.ini`，同时确认主配置已包含该目录。还需通过系统服务管理器启用 Supervisor 自身开机启动。

模板配置了：

- `numprocs=1`，避免多实例重复查询和推送。
- `autostart=true`、`autorestart=true`，随 Supervisor 启动并在异常退出后恢复。
- `stopsignal=TERM`、30 秒停止等待，配合 Go 服务的优雅退出。
- 标准输出和错误合并写入日志，单文件 20 MB，保留 5 个历史文件。
- 以 `kimi-monitor` 非 root 用户运行，不通过环境变量传递业务配置。

### 运维命令

```bash
sudo supervisorctl start kimi-monitor
sudo supervisorctl stop kimi-monitor
sudo supervisorctl restart kimi-monitor
sudo supervisorctl status kimi-monitor
sudo supervisorctl tail -f kimi-monitor
```

服务运行时仍可独立管理令牌，最迟下一轮自动生效：

```bash
printf '%s\n' 'sk-新令牌' | sudo -u kimi-monitor \
  /opt/kimi-monitor/bin/kimi-monitor \
  --config /etc/kimi-monitor/config.yaml add --stdin

sudo -u kimi-monitor /opt/kimi-monitor/bin/kimi-monitor \
  --config /etc/kimi-monitor/config.yaml list

sudo -u kimi-monitor /opt/kimi-monitor/bin/kimi-monitor \
  --config /etc/kimi-monitor/config.yaml remove 2
```

增删令牌无需重启；修改 YAML 后需要执行 `sudo supervisorctl restart kimi-monitor`。

## 数据设计

### `tokens`

保存数字 ID、打码名称、备注、HMAC 去重摘要、AES-GCM 密文和添加时间。`token_hash` 有唯一索引；打码名称不唯一，避免名称碰撞造成误判。

### `balance_records`

每次查询追加一条记录，包括查询时间、三个整数微元余额字段和错误文本。成功记录的错误为空；失败记录的余额字段为 `NULL`。环比读取同一令牌最近一次错误为空的记录。

当前版本保留全部历史，便于后续增加消耗趋势和预计耗尽时间；长期运行时可按需要增加历史清理任务。

## 安全说明

- 配置文件必须设置为 `0600`，数据库文件设置为 `0600`，其自动创建的父目录使用 `0700`。
- 数据库加密不能替代对配置文件中主密钥和 webhook URL 的保护。
- 明文令牌仅在添加和发起 API 查询时存在于进程内存中，不写入日志、报告或数据库明文列。
- 企业微信网络错误会移除完整 webhook URL，避免泄露机器人密钥。
- 请使用 `add --stdin`，并妥善保管 YAML 配置文件中的主密钥和 webhook URL。

## 验证

```bash
go test ./...
go vet ./...
```

测试覆盖 YAML 严格解析与路径解析、加密与打码、重复添加、金额精度、Kimi 响应解析、企业微信业务错误、低余额/补充/耗尽/恢复状态、失败历史衔接、Markdown 转义与消息拆分、独立管理连接增删令牌的后续轮次可见性，以及 CLI 端到端流程。
