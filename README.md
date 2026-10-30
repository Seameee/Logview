# Logview

只读的 SSH 蜜罐日志查看器：Go 单进程读取一个固定路径的日志文件，把每条记录投影成受隐私策略约束的事件，通过 SSE 推给浏览器。

- **一个二进制**。前端（Vue 3 + TypeScript strict + Vite + Tailwind CSS v4 + daisyUI 5）在内嵌产物里，运行期不需要 Node.js、Python、tmux、ttyd、ccze 或任何 PTY。
- **只读、只出一个文件**。没有文件浏览、下载、上传、shell 或出站请求；路由白名单只有静态资源、`GET /api/stream`、`GET /healthz`。
- **隐私在服务端生效**。IP 掩码与关键词替换在投影层完成，不是浏览器里的开关；被隐藏的内容不会进入 HTTP 响应或 SSE 数据。

> **v2 是不兼容重写。** 旧的 alpine + ttyd + tmux + ccze 终端镜像不再存在。迁移前请先读 [从 v1 迁移](#从-v1-迁移)，特别注意**单文件挂载必须改成目录挂载**。

## 快速开始

```bash
docker run -d \
  --name logview \
  -p 127.0.0.1:7681:7681 \
  -v /path/to/logdir:/var/log:ro \
  -e LOG_FILE=/var/log/0.log \
  -e ANONYMIZE_MODE=partial \
  ghcr.io/seameee/logview:latest
```

或者用仓库里的 [`docker-compose.yml`](docker-compose.yml)：

```bash
docker compose up -d
```

打开 `http://127.0.0.1:7681`。对外提供访问请走带 TLS 的反向代理，端口默认只绑宿主 loopback。

## 环境变量

所有配置只来自进程启动时的环境变量，HTTP 请求无法改写任何一项。空值一律按默认值处理；非空非法值会让进程**启动失败并退出**，不会悄悄退回某个默认行为。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LOG_FILE` | `/var/log/0.log` | 唯一被跟随的日志文件，必须是绝对路径。目录由你只读挂入容器。 |
| `LISTEN_ADDR` | `:7681` | HTTP 监听地址。容器内保持 7681。 |
| `ANONYMIZE_MODE` | `partial` | `partial` / `full` / `hash`，见 [IP 匿名化](#ip-匿名化)。 |
| `FILTER_WORDS` | 空 | 空白分隔的**字面**关键词，**区分大小写**，替换为固定的 `[filtered]`。 |
| `LOG_FORMAT` | `auto` | `auto` / `plain` / `fakessh`，见 [日志格式](#日志格式)。 |
| `LOG_TIMEZONE` | `UTC` | 日志时间**没有时区信息时**按此解释（IANA 名称）。带 rsyslog 外壳的行以外壳偏移为准，不受此项影响。页面会标注该假设。非法值退出。 |
| `EXPOSE_LOG_DETAILS` | `false` | 是否公开正文与版本。**这是进程级开关，不是访问控制**，见 [详情公开策略](#详情公开策略)。 |
| `MAX_SSE_CLIENTS` | `64` | 并发 SSE 连接上限，范围 1–1024。 |
| `LWS_LOG_LEVEL` | — | **已弃用**。仍可传入，但只在 stderr 提示一次，然后被忽略（它原本是 ttyd 的日志级别）。 |

`FILTER_WORDS` 含空格时必须整体加引号，否则会被 shell 和 compose 拆成多个变量：

```bash
-e FILTER_WORDS="keyword1 keyword2"     # 正确：两个关键词
-e FILTER_WORDS=keyword1 keyword2       # 错误：第二个词会被当成命令参数
```

```yaml
environment:
  FILTER_WORDS: "keyword1 keyword2"     # 正确
```

## IP 匿名化

三种模式都作用于**所有公开字段**：事件的结构化来源地址、正文里出现的地址、详情里的地址。

| 模式 | IPv4 | IPv6 |
| --- | --- | --- |
| `partial`（默认） | `203.0.113.45` → `203.0.113.xxx`（保留 /24） | `2001:db8:abcd:1234:…` → `2001:db8:abcd::/48` |
| `full` | `203.0.113.45` → `xxx.xxx.xxx.xxx` | → `xxxx:xxxx:xxxx:xxxx:xxxx:xxxx:xxxx:xxxx` |
| `hash` | `203.0.113.45` → `[H:12190CACCA27D416]` | 同左（同一套 HMAC） |

关于 `hash`：
- 哈希用 HMAC-SHA256，密钥 32 字节、**每次启动随机生成**，展示取前 16 位十六进制。
- 密钥不落盘，**重启后同一 IP 的哈希会变**，跨重启的横向对比因此不可用。这是有意为之：可持久化的密钥等于把去匿名化的钥匙留在磁盘上。
- 哈希是假名化而不是匿名化。同一个 IP 在同一进程生命周期内仍然可被关联。

## 日志格式

`LOG_FORMAT` 决定解析器如何解释每一行。解析器只做**高置信**识别，宁可少认，不猜字段。

- `auto`（默认）：尝试 fakessh 专用解析，不匹配则整行按普通文本（`message`）处理。单行是否带结构化来源，取决于它能否被无歧义地解析。
- `plain`：从不推断来源或类型，所有行都是 `message`。
- `fakessh`：强制按 fakessh 格式解析，坏行仍然容忍（记为 `unknown`，不会中断跟随）。

### 能识别什么

参考协议是 Go `log` 包的标准前缀加 `ip:port`：

```
2026/05/04 12:00:01.123456 203.0.113.45:51234
2026/05/04 12:00:02.654321 203.0.113.45:51235 SSH-2.0-libssh_0.9.6 user pass
```

- 时间戳支持 `YYYY/MM/DD HH:MM:SS.ffffff`（微秒）与秒级精度两种；按 `LOG_TIMEZONE` 解释后统一存成 UTC。解析不了就是 `timestamp: null`，接收时间单独放在 `observedAt`。
- 只有 `ip:port` 的行 → `connection` / `parseStatus: complete`。
- `ip:port` 后面还有内容 → `auth_attempt` / `parseStatus: partial`。
- 地址支持 IPv4、IPv6（`[2001:db8::1]:22` 形式带端口）与裸 IPv6（无端口）。端口为 0 或超出范围视为非法；**不会补造端口**。
- 客户端版本只在它确实是 SSH banner（`SSH-<数字>.<数字>-<软件>` 且全部可打印 ASCII）时才提取。

### rsyslog 外壳

蜜罐的 stdout 通常先被 rsyslog 收集，落盘时多一层外壳。真实长这样：

```
2026-05-04T20:00:01.123456+08:00 loghost fakessh[4242]: 2026/05/04 12:00:01.123456 203.0.113.45:51234
```

`auto` 与 `fakessh` 会自动剥掉这层 `时间 主机名 进程[PID]: ` 外壳：

- **外壳时间优先**。它自带时区偏移，是唯一无歧义的时刻；内层 `2026/05/04 12:00:01` 没有时区，只能按 `LOG_TIMEZONE` 猜，猜错就是 8 小时级别的偏移。
- 剥离是**全有或全无**的：时间必须带偏移、必须有主机名、tag 必须是 `name:` 或 `name[数字pid]:`。任一项不成立就整行原样交回内层解析，不会因为"看起来像"就切掉行首。
- 剥掉的外壳不会出现在可见文本里，所以"加不加 rsyslog"得到的投影一致（`testdata/sample-fakessh-rsyslog.log` + `internal/privacy/fixture_test.go` 的 `TestRsyslogWrapperDoesNotChangeProjection` 断言了这一点，唯一例外是内层时间无法解析时的 `timestamp`，见下一条）。
- 内层无法解析时（例如坏时间戳），带外壳的行仍能保留可靠的 `timestamp`；裸行只能留 `null`。这是"多知道一条事实"，不是猜测。
- `plain` 模式**不剥离**：它承诺原样展示，外壳也算用户要看的内容。

### 明确不做的事

fakessh 用 `log.Println` 以单空格拼接字段且不做转义，因此：

```
203.0.113.45:51235 SSH-2.0-libssh_0.9.6 user pass with spaces in it
```

**无法可靠还原**用户名与密码的分界。按空格切分或"最后一段是密码"都会产出错误的字段。所以：

- 不推断用户名、不推断密码、不推断登录成功或失败、不重建会话。
- `auth_attempt` 行只保证「这里有一次带附加内容的连接」，附加内容整体作为不透明正文处理，默认不公开。
- 裸 IPv6 地址带尾随端口（`2001:db8::1:22`）会被判为无法解析：该串本身是合法 IPv6 地址，补一个端口就是编造。需要端口请让生产者输出方括号形式。

## 详情公开策略

`EXPOSE_LOG_DETAILS` 默认 `false`。它决定**正文与版本是否进入可见投影**：

| 值 | 正文 `text` | 版本 `details.version` | 备注 |
| --- | --- | --- | --- |
| `false`（默认） | 空 | 仅 `connection` 行有 | 认证行给出一条说明：无法可靠区分版本、用户名与密码 |
| `true` | 公开（仍做地址掩码与控制字符转义） | 有 | **正文按原样公开，包含凭据** |

这些风险需要说明：

> 这是一个**进程级开关**，不是浏览器端的权限控制。设成 `true` 之后，任何能访问该端口的人都能通过 HTTP 与 SSE 看到正文，包括密码。应用没有登录、没有角色、没有按用户授权。页面上的"详情已公开"提示只是告知，不构成保护。

正确的做法是保持默认 `false`；确需排查某条记录时临时开启并在排查后关闭；对外访问一律通过反向代理做认证与 TLS，端口不直接暴露到公网。

地址掩码与详情开关无关：无论 `true` 还是 `false`，原始 IP 都不会出现在任何公开字段里。

关键词替换同样与详情开关无关，始终生效，且**区分大小写**：

```
FILTER_WORDS="LeasewebSG"   匹配 LeasewebSG，不匹配 leasewebsg
FILTER_WORDS="a.b"          匹配字面 "a.b"，不匹配 "axb"（不是正则）
```

替换为固定的 `[filtered]`，不做删除：这样「这里有一些被删掉的内容」是可见的，日志不会读起来像原文本来就没有那段。

## HTTP 接口

只有三个入口，其余一律 404；方法不对返回 405 并带 `Allow`。

| 端点 | 方法 | 说明 |
| --- | --- | --- |
| `/`、`/index.html`、`/assets/*`、`/favicon.svg`、`/favicon.ico` | GET / HEAD | 内嵌的前端产物。`/assets/*` 带一年 immutable 缓存，其余 `no-cache`。 |
| `/api/stream` | GET | SSE 事件流。 |
| `/healthz` | GET / HEAD | `{"status":"ok","source":{…},"subscribers":N}` |

所有响应都带：

```
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self';
  img-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
```

前端不使用 `v-html` / `innerHTML` / `eval` / 动态组件，日志内容只走文本插值，因此上面的严格 CSP 不需要任何 `unsafe-inline`。

`/healthz` 只表示**进程存活**。日志文件不可读时，源状态另行体现，以免把「进程活着」当成「采集正常」：

```json
{"status":"ok","source":{"state":"unreadable","message":"无法读取日志文件：请检查文件属组或 ACL","since":"…"},"subscribers":0}
```

源状态取值：`connecting`（等文件出现）、`live`、`reconnecting`（读取失败在重试）、`unreadable`（权限问题）。

### SSE 契约

```
id: 9f2c1e7a4b8d0356-142
event: snapshot
data: {"type":"snapshot","events":[…],"watermark":"9f2c1d7a4b8d0356-142", …}
```

事件 `id` 由「epoch + 单调序号」组成，epoch 每次进程启动重新随机生成。追加顺序就是去重依据，**不要拿时间戳去重**：同一微秒内可能有多条记录，日志时间也可能乱序。

- 连接时先收到一个 `snapshot`：包含最近最多 100 条事件、`watermark`、源状态、隐私配置。`watermark` 同时写在标准 `id:` 字段里。
- 带 `Last-Event-ID` 重连时，服务端补发该游标之后的事件；能补发就不发快照。
- 游标无法识别、属于旧 epoch、或已超出缓冲时，重发完整快照，`reason` 字段说明原因，客户端应以快照为准。
- `historyIncomplete: true` 表示窗口之前还有更早的记录没进缓冲（启动尾读只取最后 100 条）。
- 每约 15 秒一个注释心跳（`: heartbeat`），用于穿透中间代理的空闲超时。
- 慢客户端不会拖住别人：每个订阅者一个有界队列，队满即断开并让它重连，而不是阻塞广播。
- 订阅上限（`MAX_SSE_CLIENTS`）满时返回 503 + `Retry-After: 5`。

事件字段：

```jsonc
{
  "type": "log",
  "id": "9f2c1e7a4b8d0356-142",
  "timestamp": "2026-05-04T12:00:02.654321Z",  // 日志时间，UTC；解析不了为 null
  "observedAt": "2026-10-30T04:36:55.706323562Z", // 本进程读到它的时间，恒有值
  "kind": "auth_attempt",   // connection | auth_attempt | message | unknown
  "source": {"addr": "203.0.113.xxx", "port": 51235}, // 无法解析则为 null
  "parseStatus": "partial", // complete | partial | unrecognized
  "text": "",               // 详情关闭时为空
  "details": {"version": null, "note": "认证行详情已隐藏：无法可靠区分客户端版本、用户名与密码"}
}
```

## 文件跟随与轮转

启动时只读文件末尾的一小部分（最多 512 KiB），从中取最后 100 条完整记录：不扫全文件，也不把历史全量灌进内存。

运行期的边界：

- 单行上限 64 KiB。超长行被丢弃到下一个换行并计数，之后继续正常工作，不会因为一行异常数据永久退出。
- 半行在 EOF 处保留，等换行到齐后**只发布一次**。
- 服务端缓冲 1000 条 / 4 MiB，浏览器 500 条 / 2 MiB，都按条数和字节双配额淘汰最旧的。
- 轮转两种都支持：rename + create（先读尽旧文件再开新的，残留半行丢弃并计入诊断）、copytruncate（发现长度小于偏移就从头读）。

**关于单文件 bind mount：**

```yaml
# 不要这样
- /path/to/logfile:/var/log/0.log:ro
```

单文件挂载绑的是容器启动那一刻的 inode。日志轮转把 `0.log` 改名再新建之后，容器里看到的还是旧 inode，页面会一直停在旧内容。这是挂载语义的限制，应用无法绕过。

```yaml
# 应该这样
- /path/to/logdir:/var/log:ro
```

代价是容器能读到该目录下所有文件，所以请使用只含日志的专用目录，不要挂整个 `/var/log`。授权用文件属组或受限 ACL：

```bash
# 让容器内的 10001 组能读，而不是 chmod 777
sudo chgrp -R 10001 /path/to/logdir
sudo chmod 750 /path/to/logdir
sudo chmod 640 /path/to/logdir/0.log
```

如果日志由 rsyslog 之类的服务写成 `syslog:adm 640`，改属组可能被后续轮转重置。这时不要放宽权限，而是把宿主那个组的 GID 追加给容器进程（compose 里 `group_add: ["4"]`，GID 用 `getent group adm` 确认）：

```bash
docker run ... --group-add 4 ...
```

如果启动时文件还不存在，进程不会退出：源状态变成 `connecting`，页面显示"等待日志文件出现"，低频重试直到文件出现。

## 构建

需要一个 Node（≥ 22.12）和 Go（≥ 1.23）。一条命令完成前端类型检查、构建、把产物放进 embed 目录、跑测试、编译二进制：

```bash
./scripts/build.sh              # 完整流程
./scripts/build.sh --no-tests   # 跳过 Go 测试
```

脚本会先 `rm -rf internal/server/dist` 再复制，避免上一轮的旧 hash 资源残留被一起 embed 进去。

单独执行各部分：

```bash
cd web && npm ci && npm run build   # 前端：vue-tsc 类型检查 + vite build
cd web && npm test                  # 前端纯逻辑测试（node --test）
go test ./...                       # 后端
go test -race ./...
go vet ./...
```

（`scripts/build.sh` 已经把上述步骤串起来了；手工运行时注意先把 `web/dist/` 同步到 `internal/server/dist/`，否则内嵌的是占位页。）

前端严格度：`strict` + `noUncheckedIndexedAccess` + `exactOptionalPropertyTypes`，禁用 `any` 与 `@ts-ignore`；SSE 的 JSON 从 `unknown` 起步做运行时校验，校验不过就丢弃该帧并重连。样式是 Tailwind v4 的 CSS-first 配置（`web/src/style.css` 里 `@import "tailwindcss"` + `@plugin "daisyui"`），**没有 `tailwind.config.js`**。

两个 `dist` 都是生成目录，不提交：

- `web/dist/`：Vite 输出
- `internal/server/dist/`：复制过来供 `//go:embed` 使用（只提交一个 `.gitkeep`）

所以干净检出下 `go build ./...` 仍然能过（内嵌目录为空时页面显示"前端尚未构建"），但完整可用的二进制要先跑一次前端构建。

Go 工具链在 `Dockerfile`（`GO_IMAGE`，带摘要）和 CI（`GO_VERSION`）里固定为 **1.27.3**。不要降回 1.27.1：该版本的标准库 `net/http` 有多个已公开问题，`govulncheck ./...` 会直接报出 6 条可达漏洞，升到 1.27.3 后为 `No vulnerabilities found`。

## 从 v1 迁移

旧版本是 `alpine:latest` + ttyd + tmux + ccze + perl 的终端流。v2 是单进程 Go + 内嵌前端。以下差异会改变行为，请逐条核对：

| 项 | v1 | v2 | 需要做什么 |
| --- | --- | --- | --- |
| 挂载 | `-v /path/logfile:/var/log/0.log:ro` | 挂**专用目录** `-v /path/logdir:/var/log:ro` | **必须改**。否则轮转后页面停在旧内容。 |
| 界面 | tmux 终端 + ccze 高亮 | Vue 单页，结构化事件 + 有限窗口 | 交互方式变了；没有终端，也没有无限历史可回看。 |
| 历史 | `tail -n 100` 交给 tmux | 启动尾读最多 100 条完整记录（有字节上限） | 数量级相同，但受 512 KiB 字节上限约束。 |
| `FILTER_WORDS` 默认值 | 镜像里隐含 `LeasewebSG` | 默认为空 | 如果你的部署依赖那个隐含过滤，**显式设置** `FILTER_WORDS="LeasewebSG"`，否则原本被吃掉的词会重新出现。 |
| `FILTER_WORDS` 语义 | sed 删除匹配（且词被拼进 sed 程序，含 `/ . * &` 等字符会出错） | 字面匹配 → 替换为 `[filtered]` | 关键词不再支持正则；含特殊字符的词现在能正常工作。 |
| `FILTER_WORDS` 传参 | README 示例漏引号 | 含空格必须整体加引号 | 检查 compose / systemd 里的写法。 |
| `ANONYMIZE_MODE` | 非空非法值悄悄落到 `full` | 非空非法值**启动失败** | 修正拼写错误，不要再依赖静默降级。 |
| IPv6 | 未处理 | `partial` 保留 /48、`full` 全掩、`hash` 假名化 | 之前暴露的 IPv6 地址现在会被掩码。 |
| `hash` 格式 | `[H:%04X]`（perl 自制 CRC 截断，碰撞概率高） | `[H:…]`（16 位 HMAC-SHA256，密钥每次启动随机） | 哈希值会变，且**重启后全部改变**；不要做跨重启比对。 |
| `partial` IPv4 | 末段 → `xxx` | 末段 → `xxx`（同） | 无变化。 |
| `LWS_LOG_LEVEL` | ttyd 日志级别 0–7 | 弃用，接收但忽略并在 stderr 提示一次 | 可以从配置里删掉。 |
| 详情 | 终端显示全部内容 | 默认隐藏正文与版本 | 需要看正文时**临时**开 `EXPOSE_LOG_DETAILS=true`，并清楚它不区分访问者。 |
| 路由 | ttyd 提供终端 | 白名单三入口 | 任何依赖终端 URL 或 websocket 的集成都会失效。 |
| 端口 / 镜像名 / 架构 | `7681`、`ghcr.io/seameee/logview`、amd64+arm64+arm/v7 | 同 | 不需要改。 |
| 进程数 | ttyd + tmux + tail + ccze | 单进程 | `pids_limit` 可以调低。 |

## 安全边界

已经做的：

- 非 root（`USER 10001:10001`）、只读根文件系统、`cap_drop: ALL`、`no-new-privileges`、只读日志目录、不申请额外 capability 或宿主 socket。
- 路由白名单 + 严格 CSP + `nosniff` + `no-referrer`；静态资源先按路径段拒绝 `..`/`.`/空段，再做规范化。
- 日志内容只作为文本渲染；控制字符（C0/DEL/C1、双向覆盖字符）转成可见的 `\xNN` / `\uNNNN`，ANSI 与 OSC 序列因此只显示为字面文本，不会执行。
- 隐私投影在服务端完成，隐藏的字段不会出现在任何响应里。
- 有界资源：单行 64 KiB、服务端 1000 条 / 4 MiB、客户端 500 条 / 2 MiB、SSE 连接数上限、每订阅者有界队列、写超时 10 秒。
- SIGTERM/SIGINT 触发有界关闭（5 秒预算）；`read_only` 根文件系统下不需要写任何东西。

没有做的部分，不要当成已有能力：

- 没有身份认证、没有授权、没有按用户区分的可见性。任何人只要能连到 7681 就拥有同样的可见范围。访问控制请放在反向代理层。
- 单进程内的只读来自不提供写接口，容器层并未强制；日志目录的只读由挂载参数保证。
- 不宣称绝对安全或没有漏洞。依赖与镜像的已知问题仍需按你的节奏跟踪（`go.mod` 无第三方依赖，前端依赖锁定在 `web/package-lock.json`）。

## 已知限制

- 旧格式的凭据字段无法还原（见 [明确不做的事](#明确不做的事)）。这是生产者格式决定的。
- 裸 IPv6 带尾随端口无法解析，需要方括号形式。
- `hash` 模式跨重启不可比。
- 非 UTF-8 字节被替换为 `�`，不尝试推断编码。
- 浏览器端的实际渲染在无头 Chromium（Playwright）里验证过：1280×800 与 375×667、明暗主题、严格 CSP 下无违规、控制台无报错、`/` 聚焦筛选、详情开合与 `Escape` 关闭、无横向溢出、DOM 中无原始 IPv4。真实设备（iOS Safari、移动端输入法、屏幕阅读器语音）仍需人工确认。
- 多架构镜像的**构建**验证到 `linux/amd64`、`linux/arm64`、`linux/arm/v7` 三个平台，并核对过镜像内二进制的 ELF 架构；没有原生 ARM 运行器，ARM 上的**运行时**行为未经实测。
- 单文件 bind mount 的限制（见 [文件跟随与轮转](#文件跟随与轮转)）。

## 许可证

MIT

[![](https://img.shields.io/badge/powered_by-dsh-4D6BFE?style=flat-square&logo=deepseek&logoColor=white)](https://github.com/deepseek-ai/deepseek-harness)
