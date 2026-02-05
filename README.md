# Logview

基于 Alpine Linux 的轻量级日志展示工具，提供 Web 界面实时展示和分析日志文件。

## 功能特性

- 通过 Web 界面实时查看日志文件
- 支持 IP 地址匿名化处理（多种模式）
- 支持关键词过滤
- 日志语法高亮显示（使用 ccze）
- 基于 tmux 的会话管理
- 轻量级，基于 Alpine Linux
- 支持多客户端并发访问

## IP 地址匿名化模式

### hash 模式
将 IP 地址转换为哈希值，格式为 [H:XXXX]

### partial 模式（默认）
部分匿名化，保留 IP 地址前三段，最后一段替换为 xxx

### full 模式
完全匿名化，将 IP 地址替换为 xxx.xxx.xxx.xxx

## 环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| ANONYMIZE_MODE | partial | IP 匿名化模式：hash、partial、full |
| FILTER_WORDS | (空) | 需要过滤的关键词列表，空格分隔 |
| LOG_FILE | /var/log/0.log | 日志文件路径 |
| LWS_LOG_LEVEL | 7 | 日志级别（0-7）：0=NONE（无日志），1=FATAL（致命错误），2=ERROR（错误），3=WARN（警告），4=INFO（信息），5=DEBUG（调试），6=TRACE（跟踪），7=ALL（全部，默认） |

## 快速开始

### 使用 Docker 运行

```bash
docker run -d \
  --name logview \
  -p 127.0.0.1:7681:7681 \
  -v /path/to/logfile:/var/log/0.log:ro \
  -e ANONYMIZE_MODE=partial \
  -e FILTER_WORDS=keyword1 keyword2 \
  -e LWS_LOG_LEVEL=7 \
  --read-only \
  --tmpfs /tmp:noexec,nosuid,size=100m \
  --security-opt no-new-privileges:true \
  --cap-drop ALL \
  ghcr.io/seameee/logview:latest
```

### 使用 Docker Compose

项目提供了 `docker-compose.yml` 文件，可以直接使用：

```bash
docker-compose up -d
```

示例配置：

```yaml
services:
  logview:
    image: ghcr.io/seameee/logview:latest
    container_name: logview
    ports:
      - "127.0.0.1:7681:7681"
    volumes:
      - /path/to/logfile:/var/log/0.log:ro
    environment:
      - ANONYMIZE_MODE=partial
      - FILTER_WORDS=keyword1 keyword2
      - LWS_LOG_LEVEL=7
    read_only: true
    tmpfs:
      - /tmp:noexec,nosuid,size=100m
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    restart: unless-stopped
```

### 访问 Web 界面

启动容器后，在浏览器中访问：

```
http://localhost:7681
```

## 使用示例

### 示例 1：完全匿名化模式

```bash
docker run -d \
  --name logview \
  -p 127.0.0.1:7681:7681 \
  -v /var/log/auth.log:/var/log/0.log:ro \
  -e ANONYMIZE_MODE=full \
  --read-only \
  --tmpfs /tmp:noexec,nosuid,size=100m \
  --security-opt no-new-privileges:true \
  --cap-drop ALL \
  ghcr.io/seameee/logview:latest
```

### 示例 2：过滤敏感关键词

```bash
docker run -d \
  --name logview \
  -p 127.0.0.1:7681:7681 \
  -v /var/log/app.log:/var/log/0.log:ro \
  -e ANONYMIZE_MODE=partial \
  -e FILTER_WORDS=password token secret api_key \
  --read-only \
  --tmpfs /tmp:noexec,nosuid,size=100m \
  --security-opt no-new-privileges:true \
  --cap-drop ALL \
  ghcr.io/seameee/logview:latest
```

### 示例 3：哈希模式

```bash
docker run -d \
  --name logview \
  -p 127.0.0.1:7681:7681 \
  -v /var/log/access.log:/var/log/0.log:ro \
  -e ANONYMIZE_MODE=hash \
  --read-only \
  --tmpfs /tmp:noexec,nosuid,size=100m \
  --security-opt no-new-privileges:true \
  --cap-drop ALL \
  ghcr.io/seameee/logview:latest
```

## 安全建议

生产环境中建议：

1. 使用 `127.0.0.1:7681:7681` 绑定到本地，通过反向代理访问
2. 启用 HTTPS
3. 添加身份验证
4. 使用只读卷挂载日志文件
5. 考虑使用防火墙限制访问
