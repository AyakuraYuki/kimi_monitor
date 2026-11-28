# Kimi 令牌余额监控

一个使用 Go 编写的定时监控服务：通过 Kimi 开放平台接口查询所有令牌的余额，将加密令牌和余额历史保存到 PostgreSQL，并通过企业微信群机器人发送 Markdown 整体报告。

## 功能

- Gin 管理控制台支持本地密码和 authentik 单点登录，可查看采样、通知原文、每日消费并导出 CSV。
- 支持通过命令行或控制台添加、查看、设置备注和删除令牌。
- 令牌使用 AES-256-GCM 加密后保存；去重使用独立密钥派生的 HMAC-SHA256 摘要。
- 数据库唯一约束阻止重复添加，管理名统一为 `sk-abcd****0123`。
- 启动后立即查询，之后默认每 10 分钟进行一轮全量查询。
- 每次成功查询都保存可用、现金、代金券余额；失败查询单独保存错误，余额字段为 `NULL`。
- 每个有当日消耗的令牌都会单独展示当日总消耗，没有消耗的令牌不展示该行；指定时段还会按同样规则展示各令牌的昨日总消耗。
- 通知底部按北京时间展示当日累计消耗，并在每天 08:00–09:00、11:00–12:00、15:00–16:00 额外展示昨日总消耗；统计直接汇总 PostgreSQL 历史快照，重启后不会丢失。
- 金额以整数微元保存和比较，不使用浮点数计算环比。
- 区分余额状态与余额变动，严格识别低余额、补充、消耗、耗尽和恢复可用。
- 企业微信使用 `markdown` 消息类型；超过 4096 字节时按完整令牌块自动分批。
- 支持单次执行、并发查询、优雅退出和查询失败后的历史衔接。

## 构建

需要 Go 1.27 或更新版本，以及可访问的 PostgreSQL 数据库。数据库操作使用 GORM 和纯 Go 的 pgx 驱动，不依赖 CGO。

```bash
go build -o bin/kimi-monitor ./cmd/kimi-monitor
```

## 测试

普通单元测试可直接运行；需要数据库的集成测试通过 `KIMI_MONITOR_TEST_POSTGRES_DSN` 指定测试数据库。测试会创建随机独立 schema，并在结束后删除，不会使用默认 schema：

```bash
KIMI_MONITOR_TEST_POSTGRES_DSN='postgres://test_user:test_password@127.0.0.1:5432/test_db?sslmode=disable' go test ./...
```

未设置该环境变量时，PostgreSQL 集成测试会跳过。测试账号需要有创建和删除 schema 的权限，请勿指向生产数据库。

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

将生成的密钥填入 `security.master_key`，可按需填写默认企业微信 webhook，并限制配置文件权限：

```bash
chmod 600 config.yaml
```

监控配置示例（Web 配置见下文及 `config.example.yaml`）：

```yaml
database:
  # 数据库需提前创建；密码中的特殊字符需进行 URL 编码。
  dsn: "postgres://kimi_monitor:替换为数据库密码@127.0.0.1:5432/kimi_monitor?sslmode=disable"

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
| `database.dsn` | 是 | 无 | PostgreSQL DSN，支持 URL 或 `key=value` 格式 |
| `monitor.low_balance` | 否 | `10` | 低余额预警线，单位人民币元 |
| `monitor.interval` | 否 | `10m` | 查询间隔，使用 Go duration 格式 |
| `monitor.concurrency` | 否 | `4` | 单轮并发查询数 |
| `kimi.api_base_url` | 否 | `https://api.moonshot.cn` | 与 API Key 所属平台一致的 API 源站 |
| `wecom.webhook_url` | 否 | 无 | 企业微信群机器人默认地址，界面保存的地址优先 |

程序不会自动创建 PostgreSQL 数据库，只会自动迁移应用所需的表和索引。DSN 必须指向已经存在且可写的数据库；数据库账号需要创建和修改表的权限。远程连接建议启用 TLS（例如 `sslmode=require` 或 `verify-full`），`sslmode=disable` 仅用于可信的本地环境。

YAML 解析会拒绝未知字段。配置文件包含数据库凭据、主密钥和 webhook 密钥，因此权限必须设为 `0600`，否则命令会拒绝加载。

**主密钥不会写入数据库。必须安全、持久地保存它；丢失后已有令牌无法解密，需要删除并重新添加。不要在同一个数据库上随意更换主密钥。**

从 SQLite 版本升级时，需要将 `database.path` 改为 `database.dsn`。程序不会自动把原 SQLite 数据迁入 PostgreSQL；使用空数据库启动后，已有令牌需重新添加，首轮余额通知也会重新显示“首次记录”。

国内站和国际站的 API Key 独立。使用非默认站点时，请将 `kimi.api_base_url` 设置为该站点的 API 源站；程序会请求其 `/v1/users/me/balance` 路径。

## 管理控制台

控制台和静态资源随 Go 二进制发布。已有配置默认只启动采集。要启用本地登录，运行以下命令，在终端中输入管理员密码，然后复制输出的 bcrypt 哈希。密码支持 12 至 72 个 UTF-8 字节；密码管理器或管道可使用 `hash-password --stdin`。

```bash
./bin/kimi-monitor hash-password
```

在权限为 `0600` 的配置中加入以下内容，然后运行 `./bin/kimi-monitor --config config.yaml run`。在本机打开 `http://127.0.0.1:8080`，使用配置的管理员账号登录。输入哈希时请保留完整的美元符号及其后全部字符。

```yaml
web:
  enabled: true
  listen: "127.0.0.1:8080"
  username: "admin"
  password_hash: '替换为 hash-password 的输出'
```

### 通过 nginx 使用自己的域名

`web.listen` 省略时默认为 `127.0.0.1:8080`。同机 nginx 可直接转发到这个地址。若 nginx 位于其他主机或容器，可设置 `listen: ":8080"` 监听所有网络接口，并通过防火墙或容器网络只允许代理访问后端端口。

nginx 负责终止 HTTPS，将包含端口的原始 `Host` 转发给控制台，并用 `$scheme` 覆盖 `X-Forwarded-Proto`。控制台从 `Host` 获取公开域名及端口，接受任何直接连接方提供的单个 `X-Forwarded-Proto` 值（`http` 或 `https`），因此由部署网络限制谁能连接后端。远程浏览器需使用 HTTPS，本机回环连接可用 HTTP 访问回环地址。

以下示例放在 nginx 的 `http` 块内。将域名和证书路径换成自己的值，证书需覆盖两个域名。若该端口已有默认虚拟主机，在现有默认主机中拒绝未匹配的域名。访问日志中的 `$uri` 只记录路径，避免写入 OIDC 回调查询中的一次性授权码。[nginx 虚拟主机匹配](https://nginx.org/en/docs/http/request_processing.html) · [nginx 转发请求头](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_set_header)

```nginx
log_format kimi_path '$remote_addr [$time_local] "$request_method $uri" $status';

server {
    listen 443 ssl default_server;
    server_name _;
    ssl_reject_handshake on;
    return 444;
}

server {
    listen 443 ssl;
    server_name monitor.example.com monitor.internal.example.com;
    ssl_certificate     /etc/nginx/tls/monitor-fullchain.pem;
    ssl_certificate_key /etc/nginx/tls/monitor-key.pem;
    access_log /var/log/nginx/kimi-monitor.log kimi_path;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $http_host;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

新增域名时，更新 DNS、证书与 nginx，启用 SSO 时还需登记对应的回调地址。登录会话和进行中的 OIDC 登录绑定访问源，不同域名需要分别登录。会话由单个服务实例保存，8 小时后到期，退出登录或重启进程即失效。同一代理后的用户共享基于直接连接来源的登录限流额度。

### 使用 authentik 登录

1. 在 authentik 创建应用及关联的 OAuth2/OpenID Provider。客户端类型选 **Confidential**，配置签名密钥，选择 `openid`、`profile` scope mappings。控制台采用 Authorization Code 流程并发送 PKCE S256 challenge。[authentik OAuth 2.0 Provider](https://docs.goauthentik.io/add-secure-apps/providers/oauth2/)
2. 在 Provider 的 **Redirect URIs** 下为每个入口添加一条 **Strict** 授权回调。例如上面的两个域名分别填写 `https://monitor.example.com/auth/oidc/callback` 和 `https://monitor.internal.example.com/auth/oidc/callback`。使用非默认 HTTPS 端口时也要填入端口。
3. 在 **Applications → Applications** 打开监控应用，进入 **Policy / Group / User Bindings → Create or bind… → Bind a group**，选择允许管理令牌和通知的组，如 `kimi-monitor-admins`。也可以在同一位置绑定已有的授权策略。authentik 的默认设置允许所有用户访问没有绑定的应用。[authentik 应用访问绑定](https://docs.goauthentik.io/add-secure-apps/applications/manage_apps#use-bindings-to-control-access)
4. 将 Provider 的完整 issuer（保留结尾斜杠）、Client ID 和 Client Secret 填入 YAML 并重启。仅使用 SSO 时，将本地用户名与密码哈希留空。SSO 用户的访问权限由 authentik 的应用绑定决定，无需在 ID Token 中映射 `groups` 声明。登录成功的用户都拥有令牌和通知管理权限。

```yaml
web:
  enabled: true
  listen: "127.0.0.1:8080"
  oidc:
    issuer: "https://sso.example.com/application/o/kimi-monitor/"
    client_id: "应用的 Client ID"
    client_secret: "应用的 Client Secret"
```

**从旧版升级时**，若此前通过 `web.oidc.allowed_groups` 限制访问，先将对应的组绑定到 authentik 应用并确认其准入规则。旧 YAML 中的 `web.public_url`、`web.trusted_proxies` 和 `web.oidc.allowed_groups` 均须删除，严格解析会拒绝这些已移除的字段。

### 查看、管理和导出

- **监控概览**展示最近一次采集状态及成功余额。采集失败后仍可查看上次成功的金额和时间。通知预览展示企业微信已确认接收的原文；分片发送时记录实际送达的片数。升级后首次发送成功即开始保留预览。
- **令牌管理**支持添加推理 API Key、修改或清空备注、暂停或恢复单个令牌的 API 额度查询，以及确认删除。暂停从下一轮采集开始生效，不再调用该令牌的余额接口，历史余额与消耗记录保留；恢复后从下一轮继续采集。已有 Key 始终只显示打码名；删除沿用 CLI 行为，同时删除其余额与消费历史。已发送的通知保留发送时的原文。
- **每日消费**按北京时间自然日查询，起止日期均包含，单次最多 366 天。表格和 CSV 共用查询；有成功采样且未观察到下降时金额为零，全天无成功采样时 CSV 金额留空。导出保留六位小数，使用当前令牌备注。以公式字符开头的备注会加文本前缀，便于安全地用电子表格打开。
- **通知设置**显示当前生效来源及配置文件是否提供兜底。界面保存的 webhook 加密存入数据库，在下一轮推送时生效；清除界面配置即恢复 `wecom.webhook_url`。两处均为空时仍采集余额。完整地址及机器人密钥不会通过读取接口返回。

### 消费口径与后续对账

每个令牌的消费按相邻成功采样之间可用余额的下降量估算，下降归入后一次采样所在的北京时间日期。余额接口返回账户余额：同一账户的多个 Key 可以观察到同一笔下降，采样间发生的充值也可能抵消消费。消费接口标记数据来源为 `balance_snapshots`，并返回本地稳定的 `token_id`。[Kimi 查询余额](https://platform.kimi.com/docs/api/balance)

后续接入正式账单时，可新增组织级采集器，用独立的 `sk-admin-*` 和 `billing:read` 权限读取账单。需要按 Key 匹配时另存官方 Key ID 与本地 ID 的映射，打码名可能重复。对账记录应独立保存，按组织、日期和官方账单维度幂等入库，再与快照估算比较。官方金额以毫分为单位（1 毫分 = 10 微元），时间窗左闭右开，T 日账单自 T+1 日北京时间 07:00 起可查。当前版本只接收推理密钥进行余额监控。[Kimi Admin API 概述](https://platform.kimi.com/docs/admin-api/overview)

## 令牌管理

推荐使用标准输入添加，避免令牌进入 shell 历史和进程参数。交互式使用时，输入令牌后按 Enter 即可，无需再按 Ctrl+D；通过管道输入时仍兼容 EOF 结束：

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

服务每一轮都会重新读取 PostgreSQL 的 `tokens` 表，不在内存中缓存令牌清单。独立 CLI 通过同一数据库写入后：

- 新增令牌最迟在下一轮自动加入查询和推送，无需重启服务。
- 删除令牌最迟在下一轮停止查询。
- 如果管理操作发生在某一轮查询进行中，该轮可能沿用轮次开始时的清单，下一轮会读取最新清单。
- 服务与短时管理命令使用独立的 PostgreSQL 连接，可以同时访问同一数据库。

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
> 检查时间：2026-11-18 08:30:00 CST
> 令牌：2 个 ｜ 🟢 正常 1 ｜ 🟡 预警 1 ｜ 🔴 用尽 0 ｜ ⚠️ 异常 0
> 可用余额合计：**¥54.588940** ｜ 本轮补充：0 个
> 低余额预警线：¥10.000000

### #1 `sk-abcd****5678`
> 备注：生产环境主账号
<font color="info">🟢 正常</font>　📉 消耗 **¥0.100000**
> 可用余额：**¥49.588940**
> 当日总消耗：**¥0.100000**
> 昨日总消耗：**¥12.345678**
> 上次可用：¥49.688940（2026-11-18 08:20:00 CST）
> 现金：¥3.000010 ｜ 代金券：¥46.588930

### #2 `sk-wxyz****9012`
<font color="warning">🟡 余额不足</font>　可用余额：**¥5.000000**

### 消耗统计（北京时间）
> 今日累计消耗（00:00 至 08:30:00）：**¥0.100000**
> 昨日总消耗（2026-11-17）：**¥12.345678**
```

## Supervisor 部署

配置模板位于 `deploy/supervisor/kimi-monitor.conf`，默认使用以下路径：

| 用途 | 路径 |
| --- | --- |
| 二进制 | `/opt/kimi-monitor/bin/kimi-monitor` |
| YAML 配置 | `/etc/kimi-monitor/config.yaml` |
| PostgreSQL 连接 | 由配置文件中的 `database.dsn` 指定 |
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
sudo install -d -o kimi-monitor -g kimi-monitor -m 0750 /var/log/kimi-monitor

sudo install -o kimi-monitor -g kimi-monitor -m 0600 \
  config.example.yaml /etc/kimi-monitor/config.yaml
```

编辑 `/etc/kimi-monitor/config.yaml`，填写数据库、主密钥及可选的默认通知地址：

```yaml
database:
  dsn: "postgres://kimi_monitor:数据库密码@数据库地址:5432/kimi_monitor?sslmode=require"
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

消耗统计直接由 PostgreSQL 按令牌分组汇总这些持久化快照，不在进程内保存累计值，因此重启服务或执行 `run --once` 都不会丢失既有统计。统计时按记录顺序寻找同一令牌的上一成功快照，只累计可用余额的下降量；失败记录不会打断衔接，余额补充不计为消耗。日期边界使用北京时间，当日首条成功快照会与午夜前最近一次成功快照比较。每个令牌仅在对应消耗大于 `0` 时展示独立的“当日总消耗”或“昨日总消耗”行，通知底部仍展示所有令牌的合计。每天 `08:00:00` 至 `08:59:59`、`11:00:00` 至 `11:59:59`、`15:00:00` 至 `15:59:59` 的通知还会查询并展示前一自然日的完整消耗及各令牌消耗。

当前版本保留全部历史，用于按自然日汇总消耗，也便于后续增加消耗趋势和预计耗尽时间；长期运行时可按需要增加历史清理任务。

## 安全说明

- 配置文件请设置为 `0600`，并限制 PostgreSQL 应用账户的权限。
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
