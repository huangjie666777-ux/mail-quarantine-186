# SMTP 联调收信后端

这是一个仅用于研发联调的本机 SMTP 收信、POP3 取信与只读 HTTP 查询服务。服务不向外转发邮件，也不解析或改写 MIME；`DATA` 中完成点透明解码后的原始字节会直接存入 SQLite。

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
- 仅认证后的 `QUIT` 会在单个 SQLite 事务中删除该收件人的标记项，失败回滚并报 `-ERR`；断连或超时不删除。同封邮件的其他收件人不受影响，HTTP 归档保留完整信封。
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

所有接口只接受 `GET`，不允许查询字符串。收件人必须在配置中，ID 必须是服务端生成的 32 位小写十六进制字符串。

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

## 开发验证

```sh
go test ./...
go build ./...
```
