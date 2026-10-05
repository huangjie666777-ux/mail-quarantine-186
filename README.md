# SMTP 联调收信后端

这是一个用于研发联调的本机 SMTP 收信、POP3 取信与 HTTP 归档/隔离处置服务。服务不向外转发邮件，也不解析或改写 MIME；`DATA` 中完成点透明解码后的原始字节会直接存入 SQLite。

## 配置

所有配置通过环境变量提供：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SMTP_ADDR` | `127.0.0.1:2525` | SMTP 监听地址，只允许 `127.0.0.1` 或 `::1` |
| `HTTP_ADDR` | `127.0.0.1:8080` | HTTP 监听地址，只允许 `127.0.0.1` 或 `::1` |
| `DB_PATH` | `mail.db` | SQLite 数据库路径 |
| `ALLOWED_RECIPIENTS` | 无，必填 | 逗号分隔的允许收件人；域名大小写不敏感，本地部分大小写敏感 |
| `MAX_CONNECTIONS` | `100` | 每种服务各自允许的同时连接数 |
| `READ_TIMEOUT_SECONDS` | `60` | 单次网络读取和 HTTP 处理超时 |
| `MAX_LINE_BYTES` | `4096` | SMTP 命令或邮件单行最大字节数（含 CRLF） |
| `MAX_RECIPIENTS` | `100` | 单封邮件最多不同有效收件人数 |
| `MAX_MAIL_BYTES` | `10485760` | 单封邮件原文最大字节数 |
| `POP3_ENABLE` | 关闭 | 设为 `1`/`true` 启用 POP3 取信；不设置时保持旧行为 |
| `POP3_ADDR` | `127.0.0.1:1100` | POP3 监听地址，只允许 `127.0.0.1` 或 `::1` |
| `POP3_PASSWORD` | 无，启用时必填 | POP3 `PASS` 校验密码，所有邮箱共用 |

启动示例：

```sh
ALLOWED_RECIPIENTS='dev@example.com,qa.Name@example.com' \
DB_PATH=/tmp/devmail.db \
SMTP_ADDR=127.0.0.1:2525 \
HTTP_ADDR=127.0.0.1:8080 \
POP3_ENABLE=1 \
POP3_ADDR=127.0.0.1:1100 \
POP3_PASSWORD=pw123 \
go run .
```

## SMTP 行为

- 支持 `HELO`、`EHLO`、`MAIL FROM`、`RCPT TO`、`DATA`、`RSET`、`NOOP`、`QUIT`。
- 必须先问候，再提交发件人和至少一个配置内收件人，才允许 `DATA`。
- 允许空发件人：`MAIL FROM:<>`。
- 只接受 SMTP 信封收件人；不会读取或推断邮件 `To` 头。
- 重复信封收件人只保存一次。
- 命令必须使用严格 CRLF；支持 TCP 分包和连续命令。
- `DATA` 仅以独占一行的 `.` 加 CRLF 结束，并移除行首转义点。
- 半封邮件断连、行过长或邮件过大时不保存；异常连接会被关闭。
- 消息与收件人关联在同一个 SQLite 事务中提交，提交成功后才返回 `250`。
- 服务在进入 `DATA` 时固定整份规则及版本；逐收件人独立匹配并在同一事务保存原信封、原文、命中规则、规则版本和该收件人的投递状态。
- 没有命中规则时默认放行；命中隔离规则时 SMTP 仍返回成功，但该邮件仅对对应收件人暂时不可通过 POP3 取信。

Python SMTP 客户端示例：

```sh
python3 - <<'PY'
import smtplib
from email.message import EmailMessage

message = EmailMessage()
message['Subject'] = 'local debug'
message['To'] = 'dev@example.com'
message.set_content('hello capture\n')

with smtplib.SMTP('127.0.0.1', 2525) as client:
    client.send_message(message, from_addr='sender@example.org', to_addrs=['dev@example.com'])
PY
```


## POP3 行为

- 按 RFC 1939 子集支持 `USER`、`PASS`、`STAT`、`LIST`、`UIDL`、`RETR`、`DELE`、`RSET`、`NOOP`、`QUIT`；不支持 TLS 与 APOP。
- `USER` 为允许收件地址（复用 SMTP 收件人规则：域名大小写不敏感，本地部分大小写敏感），`PASS` 为 `POP3_PASSWORD`；未认证不能取信。
- 认证成功后独占锁定该邮箱，按接收时刻和 ID 升序冻结快照，从 1 编号；同邮箱第二会话立即 `-ERR` 拒绝，不影响其他邮箱和 SMTP 收信；新到邮件下次登录可见。
- `UIDL` 使用消息 ID，跨会话和重启稳定；`LIST`/`UIDL` 支持单条与整表。
- `RETR` 返回原始邮件并做点透明转义，以 `.` 行结束；`STAT`/`LIST` 大小为原 CRLF 字节数，不含转义点和结束行。
- `DELE` 仅做标记，后续查询排除但不重编号；`RSET` 撤销全部标记。
- 仅认证后的 `QUIT` 会在单个 SQLite 事务中把该收件人的标记项置为 `deleted`，失败回滚并报 `-ERR`；断连或超时不删除。HTTP 归档和完整信封继续保留，之后不会重放。同封邮件的其他收件人不受影响。
- 人工放行发生在旧 POP3 会话之外，不改变该会话已经冻结的快照；重新认证后邮件可见，UIDL 始终为消息 ID。
- 命令必须严格 CRLF，支持分包和连续命令；连接数、行长和读取期限复用 `MAX_CONNECTIONS`、`MAX_LINE_BYTES`、`READ_TIMEOUT_SECONDS`。

Python POP3 客户端示例：

```sh
python3 - <<'PY'
import poplib

with poplib.POP3('127.0.0.1', 1100) as client:
    client.user('dev@example.com')
    client.pass_('pw123')
    print(client.stat())
    print(client.list())
    response, lines, _ = client.retr(1)
    print(bytes().join(lines).decode())
    client.quit()  # quit commits DELE marks
PY
```
## HTTP API

归档接口使用 `GET`，规则整表替换使用 `PUT`，人工处置使用 `POST`；所有接口均不允许查询字符串。收件人必须在配置中，ID 必须是服务端生成的 32 位小写十六进制字符串。

### 隔离规则

读取当前整表及版本：

```sh
curl -s 'http://127.0.0.1:8080/api/rules'
```

响应：

```json
{
  "version": 1,
  "rules": [
    {
      "id": "block-evil",
      "recipient": "dev@example.com",
      "sender_domain": "evil.example",
      "action": "quarantine"
    }
  ]
}
```

整表替换：

```sh
curl -s -X PUT 'http://127.0.0.1:8080/api/rules' \
  -H 'Content-Type: application/json' \
  -d '{
    "expected_version": 0,
    "rules": [
      {"id":"block-evil","recipient":"dev@example.com","sender_domain":"evil.example","action":"quarantine"},
      {"id":"allow-empty","recipient":"dev@example.com","sender_domain":"","action":"allow"},
      {"id":"block-all","recipient":"qa.name@example.com","sender_domain":"*","action":"quarantine"}
    ]
  }'
```

- `expected_version` 必须等于当前版本；过期替换返回 `409` 和服务端当前版本，不修改任何规则。
- 规则数组顺序即优先级，首条同时匹配收件地址和信封发件域的规则生效。
- `id` 在一次提交中必须唯一；`recipient` 必须是允许收件地址。
- `sender_domain` 大小写不敏感；`*` 匹配所有发件域且包含空发件人；空字符串只匹配空发件人。
- `action` 为 `allow` 或 `quarantine`；默认（无命中）为放行。规则更新只影响后续邮件，不追溯重判历史邮件。

列出某收件人的邮件元信息：

```sh
curl -i 'http://127.0.0.1:8080/api/recipients/dev@example.com/messages'
```

按 ID 查看实际信封：

```sh
curl -s 'http://127.0.0.1:8080/api/recipients/dev@example.com/messages/<id>'
```

下载未改动的 `.eml` 原文：

```sh
curl --output message.eml 'http://127.0.0.1:8080/api/recipients/dev@example.com/messages/<id>/eml'
```

JSON 元信息包含：`id`、`received_at`、`sender`、`recipients`、`size`。

归档元信息还会按该收件人返回 `status`、`action`、`rule_id`、`reason` 和人工处置时间 `decided_at`；`recipients` 始终保留完整原信封，即使某收件人后来取信删除也不改变其他收件人的归档。

### 隔离处置

列出当前仍待处理的隔离项：

```sh
curl -s 'http://127.0.0.1:8080/api/recipients/dev@example.com/quarantine'
```

按“邮件 ID + 收件人”放行或丢弃：

```sh
curl -s -X POST 'http://127.0.0.1:8080/api/recipients/dev@example.com/messages/<id>/release'
curl -s -X POST 'http://127.0.0.1:8080/api/recipients/dev@example.com/messages/<id>/discard'
```

成功响应为持久状态和时间，例如：

```json
{"status":"released","decided_at":"2026-10-05T01:02:03.456789Z"}
```

- 只有 `quarantined` 项可转换；并发请求只允许一个决定落库。
- 重复做相同决定是幂等的，返回同一个持久状态与时间。
- 放行后再丢弃、或丢弃后再放行返回 `409`；非隔离项不能通过处置接口转换。
- 丢弃后 POP3 永远不可见，但消息原文和完整信封仍可通过该收件人的 HTTP 归档查询。

### 收件人状态

| 状态 | 语义 | POP3 可见 |
| --- | --- | --- |
| `deliverable` | 默认放行或命中 `allow` | 是 |
| `quarantined` | 命中隔离规则，等待人工处理 | 否 |
| `released` | 人工放行，下一次认证进入快照 | 是 |
| `discarded` | 人工丢弃，不删除归档 | 否 |
| `deleted` | POP3 `QUIT` 后标记取信删除，不删除归档 | 否 |

## 开发验证

```sh
go test ./...
go build ./...
```
