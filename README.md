# deepseek-search-mcp

把 [DeepSeek Search / Fetch API](https://api-docs.deepseek.com/zh-cn/guides/search) 封装成公共 MCP 服务的 Go 实现。

它本身不持有任何 API Key，也不做计费：每个调用方在 MCP 连接的 `Authorization` 头中携带自己的 DeepSeek API Key，服务把该 Key 原样透传给 DeepSeek，并把结果转换成标准 MCP 工具响应。

```text
┌────────────┐   Authorization: Bearer <用户自己的 Key>   ┌──────────────────────┐
│ MCP Client │ ─────────────────────────────────────────▶ │ deepseek-search-mcp  │
└────────────┘                                            │  (无状态 / 不存 Key) │
                                                          └──────────┬───────────┘
                                                                     │ Authorization: Bearer <同一个 Key>
                                                                     ▼
                                                          ┌──────────────────────┐
                                                          │ api.deepseek.com     │
                                                          │ /v1/tools/search     │
                                                          │ /v1/tools/fetch      │
                                                          └──────────────────────┘
```

## 特性

- **公共中转、用户自带 Token**：鉴权头 `Authorization: Bearer <DeepSeek API Key>` 必填，服务不存储、不记录 Token。
- **标准 MCP Streamable HTTP**：默认无状态（`MCP_STATELESS=true`），可水平扩容，不需要粘性会话。
- **两个工具**：`search`（互联网搜索）和 `fetch`（网页正文抓取），完整支持上游参数。
- **结构化输出**：同时返回 `structuredContent`（便于程序处理）和 Markdown 文本（便于模型阅读）。
- **服务端防护**：请求体大小限制、上游响应大小限制、超时、CORS、访问日志（不含请求头和 Token）、panic 恢复、优雅退出。
- **无本地 SSRF**：`fetch` 的 URL 由 DeepSeek 抓取，本服务不会向任意 URL 发起请求，只固定访问 `DEEPSEEK_BASE_URL`。

## 工具

### `search`

调用 `POST /v1/tools/search`。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `query` | string | 是 | 搜索词，1–128 个字符，不能全为空白，支持搜索语法 |
| `count` | integer | 否 | 返回条数，1–50，默认 10 |
| `ori_question` | string | 否 | 用户原始问题（≤128 字符），帮助理解搜索意图 |
| `localization` | string | 否 | 用于优先返回本地化结果的 IP 地址 |
| `need_content` | boolean | 否 | 为 `true` 时在每条结果的 `content` 中返回网页正文，默认 `false` |

返回 `results[]`，每条包含：

| 字段 | 说明 |
|---|---|
| `rerank_score` | 排序分数 0–3，综合相关性、权威性、时效性；≥1.5 通常为优质结果 |
| `title` / `url` | 标题与链接 |
| `snippet` | 约 100–200 字符的短摘要 |
| `summary` | 约 600–800 字符的长摘要 |
| `content` | 网页正文，仅 `need_content=true` 且正文可用时返回 |
| `site_name` / `site_icon` | 站点名称与图标 URL |
| `date` | 发布时间（UTC，RFC 3339），未知时省略 |
| `authority` | 权威性 0–4（-1 未知），≥3 为极权威 |

搜索语法：

| 语法 | 示例 | 作用 |
|---|---|---|
| `site:` | `site:github.com` | 限定站点；多个用空格分隔并经 OR 连接 |
| `-site:` | `-site:example.com` | 排除站点 |
| `-` | `-广告` | 排除含指定关键词的网页 |
| `-"..."` | `-"release notes"` | 排除含指定短语的网页 |
| `"..."` | `"精确短语"` | 精确匹配短语 |
| `lang:` | `lang:zh,en` | 限定语言 |
| `after:` | `after:2025` / `after:2025-01-01` | 限定发布日期之后 |
| `before:` | `before:2026` / `before:2026-06-01` | 限定发布日期之前 |

示例：`"DeepSeek 模型 site:github.com OR site:huggingface.co lang:zh after:2025"`

### `fetch`

调用 `POST /v1/tools/fetch`。

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `url` | string | 是 | HTTP/HTTPS URL，≤8192 字节，不能包含用户名或密码 |

返回 `title`、`content`（Markdown 正文）以及实际抓取的 `url`。目标站点可能拒绝访问或页面没有可提取正文，此时上游返回 404。

## 快速开始

要求 Go 1.25+。

```bash
go build -o bin/deepseek-search-mcp .
./bin/deepseek-search-mcp
```

默认监听 `:8080`，MCP 端点为 `http://127.0.0.1:8080/mcp`。服务不读取 `DEEPSEEK_API_KEY` 环境变量；Token 由每个客户端在请求头中提供。

```bash
curl -sS http://127.0.0.1:8080/healthz
# {"status":"ok"}
```

不带鉴权头访问 MCP 端点会得到 401：

```bash
curl -i -X POST http://127.0.0.1:8080/mcp \
  -H 'Content-Type: application/json' \
  -d '{}'
# HTTP/1.1 401 Unauthorized
# WWW-Authenticate: Bearer realm="deepseek-search-mcp"
```

带自己的 Key 初始化：

```bash
curl -sS http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer $DEEPSEEK_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {
      "protocolVersion": "2025-11-25",
      "capabilities": {},
      "clientInfo": {"name": "curl", "version": "1.0.0"}
    }
  }'
```

直接调用 `search`（默认无状态模式，不需要保存会话 ID）：

```bash
curl -sS http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer $DEEPSEEK_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{
    "jsonrpc": "2.0",
    "id": 2,
    "method": "tools/call",
    "params": {
      "name": "search",
      "arguments": {
        "query": "DeepSeek V4 site:deepseek.com",
        "count": 5,
        "need_content": false
      }
    }
  }'
```

调用 `fetch`：

```bash
curl -sS http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer $DEEPSEEK_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{
    "jsonrpc": "2.0",
    "id": 3,
    "method": "tools/call",
    "params": {
      "name": "fetch",
      "arguments": {"url": "https://api-docs.deepseek.com/zh-cn/guides/search"}
    }
  }'
```

## 客户端配置

所有客户端都需要在连接头中设置 `Authorization: Bearer <你的 DeepSeek API Key>`。下面用 `https://your-domain.example/mcp` 代表部署后的地址。

### Claude Code

```bash
claude mcp add --transport http deepseek-search https://your-domain.example/mcp \
  --header "Authorization: Bearer $DEEPSEEK_API_KEY"
```

### Cursor

`~/.cursor/mcp.json` 或项目 `.cursor/mcp.json`：

```json
{
  "mcpServers": {
    "deepseek-search": {
      "url": "https://your-domain.example/mcp",
      "headers": {
        "Authorization": "Bearer ${env:DEEPSEEK_API_KEY}"
      }
    }
  }
}
```

### VS Code

`.vscode/mcp.json`：

```json
{
  "inputs": [
    {
      "id": "deepseek-key",
      "type": "promptString",
      "description": "DeepSeek API Key",
      "password": true
    }
  ],
  "servers": {
    "deepseek-search": {
      "type": "http",
      "url": "https://your-domain.example/mcp",
      "headers": {
        "Authorization": "Bearer ${input:deepseek-key}"
      }
    }
  }
}
```

### 其他支持 Streamable HTTP 的客户端

```json
{
  "mcpServers": {
    "deepseek-search": {
      "type": "streamable-http",
      "url": "https://your-domain.example/mcp",
      "headers": {
        "Authorization": "Bearer sk-你的-Key"
      }
    }
  }
}
```

部分客户端把 `type` 写成 `"http"`，按客户端文档为准。

### 仅支持 stdio 的客户端

可以用 [`mcp-remote`](https://www.npmjs.com/package/mcp-remote) 做 stdio 到 Streamable HTTP 的桥接：

```bash
npx -y mcp-remote https://your-domain.example/mcp \
  --header "Authorization: Bearer $DEEPSEEK_API_KEY"
```

## 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `ADDR` | `:8080` | HTTP 监听地址；生产环境建议 `127.0.0.1:8080` 并置于反向代理之后 |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com` | 上游地址；仅测试时才应修改 |
| `DEEPSEEK_TIMEOUT` | `60s` | 单次上游请求超时 |
| `MAX_UPSTREAM_RESPONSE_BYTES` | `33554432`（32 MiB） | 上游响应体大小上限 |
| `MCP_MAX_BODY_BYTES` | `1048576`（1 MiB） | MCP 请求体大小上限 |
| `MCP_STATELESS` | `true` | 无状态模式；公共部署推荐保持开启 |
| `MCP_JSON_RESPONSE` | `true` | 用 `application/json` 返回，而不是 SSE；兼容性更好 |
| `MCP_SESSION_TIMEOUT` | `30m` | 有状态模式下的空闲会话超时 |
| `CORS_ALLOWED_ORIGINS` | `*` | 允许的浏览器 Origin，逗号分隔；不需要浏览器客户端时可收紧 |
| `SHUTDOWN_TIMEOUT` | `10s` | 优雅退出等待时间 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

默认的无状态模式适合多实例水平扩容：每个请求都自带鉴权头，服务端不保存会话。若设置 `MCP_STATELESS=false` 启用有状态会话，会话保存在单进程内存中，多实例部署需要按 `Mcp-Session-Id` 做粘性会话。

## 部署

### Docker

```bash
docker build -t deepseek-search-mcp .
docker run --rm -p 127.0.0.1:8080:8080 deepseek-search-mcp
```

构建时指定版本：

```bash
docker build --build-arg VERSION=1.0.0 -t deepseek-search-mcp:1.0.0 .
```

### 反向代理

生产环境请始终用 TLS 终止代理暴露服务，不要直接把明文 HTTP 暴露到公网。代理需要转发 `Authorization`、`Content-Type`、`Accept`、`Mcp-Session-Id`、`MCP-Protocol-Version`、`Last-Event-ID` 等头。

Caddy：

```caddyfile
mcp.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Nginx（默认 JSON 响应模式）：

```nginx
server {
    listen 443 ssl;
    server_name mcp.example.com;

    client_max_body_size 1m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 120s;
    }
}
```

如果设置 `MCP_JSON_RESPONSE=false` 启用 SSE，请额外关闭代理缓冲并放宽读超时：

```nginx
proxy_buffering off;
proxy_cache off;
proxy_read_timeout 3600s;
```

### systemd

```ini
[Unit]
Description=DeepSeek Search MCP relay
After=network-online.target
Wants=network-online.target

[Service]
User=deepseek-mcp
ExecStart=/opt/deepseek-search-mcp/deepseek-search-mcp
Environment=ADDR=127.0.0.1:8080
Environment=LOG_LEVEL=info
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
```

## 发布

[`.github/workflows/release.yml`](.github/workflows/release.yml) 在以下情况自动运行：

- 推送 `v*` 标签，例如 `v0.1.0`；
- 在 GitHub 上发布 Release（`release: published`）。

工作流会先运行 `go test -race ./...` 和 `go vet ./...`，然后构建 `linux/amd64` 镜像，导出为 tar 并上传到对应 Release：

```text
deepseek-search-mcp-v0.1.0-linux-amd64.tar
deepseek-search-mcp-v0.1.0-linux-amd64.tar.sha256
```

触发方式：

```bash
git tag v0.1.0
git push origin v0.1.0
```

如果对应 Release 不存在，工作流会自动创建；如果已存在，则用 `--clobber` 更新同名资产，因此对同一个标签重复运行是安全的。带 `-` 的标签（如 `v0.1.0-rc1`）在新建 Release 时会标记为 pre-release。

下载并加载镜像：

```bash
sha256sum -c deepseek-search-mcp-v0.1.0-linux-amd64.tar.sha256
docker load -i deepseek-search-mcp-v0.1.0-linux-amd64.tar
docker run --rm -p 127.0.0.1:8080:8080 deepseek-search-mcp:v0.1.0
```

## 错误与限流

DeepSeek 错误会作为 MCP 工具错误（`isError: true`）返回，并带有状态码与处理提示：

| 上游状态 | 含义 |
|---|---|
| 400 | 请求参数不合法 |
| 401 | API Key 缺失或无效 |
| 402 | 账户余额不足 |
| 403 | Fetch 被安全策略拒绝 |
| 404 | Fetch 未获取到可用网页内容 |
| 408 / 504 | 超时，可稍后重试 |
| 413 | 请求体过大 |
| 429 | 超过 Search / Fetch QPS 限制（各 5 QPS，按账号计算） |
| 5xx | DeepSeek 服务内部错误 |

- Search 与 Fetch 均按成功请求次数计费（当前每 1,000 次 5 元，以 DeepSeek 官方文档为准），费用记在 Token 所属账号。
- 失败、无结果、超时不计费；`count` 不影响单次价格。
- 429、408、5xx 可以等待后重试；402 需要充值；其他 4xx 在未修改请求时不要重试。

## 安全说明

- 服务端不保存 Token。Token 只存在于单次请求的内存中，并随该请求转发给 DeepSeek。
- Token 会以明文到达本服务（TLS 通常在反向代理处终止）。只应使用你信任的部署实例；实例运营者在进程内存中理论上可以接触到 Token，本实现不记录、不持久化。
- 访问日志不记录请求头，也不会输出 Token；上游错误消息中若出现 Token 会被替换为 `[REDACTED]`。
- 请确保反向代理、MCP 客户端和日志采集系统不会记录 `Authorization` 头。
- `fetch` 的 URL 仅做格式校验，实际抓取由 DeepSeek 完成；本服务不会向用户提供的 URL 发起请求，因此不存在本机 SSRF 面。
- 公网部署必须使用 HTTPS；Token 在明文 HTTP 下会被窃听。
- 为兼容同机反向代理（请求源地址看起来是 127.0.0.1），服务关闭了 MCP SDK 的 localhost DNS-rebinding 保护；安全性由强制的 Bearer 鉴权保证。
- `CORS_ALLOWED_ORIGINS=*` 便于浏览器客户端接入；若只给服务端或桌面客户端使用，建议收紧为具体 Origin。
- 每个调用方只能消耗自己的 DeepSeek 额度；本服务没有内置的全局 API Key，缺少鉴权头的请求会在入口直接被拒绝。
- 公网部署建议在反向代理层增加按 IP 或 Token 的限流与审计，以抵御滥用。

## 开发

```bash
make test        # go test ./...
make test-race   # go test -race ./...
make vet         # go vet ./...
make build       # 构建到 bin/deepseek-search-mcp
make run         # 本地运行
make docker      # 构建 Docker 镜像
```

目录结构：

```text
.
├── main.go                     # 启动、依赖装配、优雅退出
├── internal/
│   ├── auth/                   # Authorization: Bearer 解析
│   ├── config/                 # 环境变量配置
│   ├── deepseek/               # DeepSeek Search/Fetch 客户端与错误映射
│   ├── httpserver/             # 鉴权、CORS、日志、健康检查、panic 恢复
│   └── mcpserver/              # MCP 工具定义、输入输出 schema、结果渲染
├── main_test.go                # MCP 全链路测试（客户端 → 鉴权 → 工具 → mock 上游）
├── Dockerfile
└── Makefile
```

## 说明

本项目是社区封装，与 DeepSeek 官方无隶属关系。使用 Search / Fetch 服务即表示你同意 DeepSeek 的服务条款与计费规则。
