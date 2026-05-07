# Go Agents Proxy

基于 Go 编写的 Agents API 代理服务器。接收 Anthropic、OpenAI, Google 格式的请求，转发到配置的上游服务商（OpenAI、Google/Gemini、Anthropic），支持自动格式转换、服务商故障自动切换以及内置的 Web 管理后台。

## 核心特性

1. **配置驱动路由**：在 `config.yaml` 中定义路由、模型和服务商回退链
2. **多服务商支持**：支持 OpenAI、Google（Gemini）和 Anthropic API 服务商
3. **自动故障切换**：某个服务商异常时，自动尝试配置列表中的下一个目标
4. **API 格式转换**：自动将 Anthropic API 格式转换为 OpenAI/Google 格式，并将响应转回 Anthropic 格式
5. **流式响应**：完整支持 SSE 流式响应，包括 `tool_use` 事件
6. **工具调用**：支持 Anthropic 的 `tool_use` 和 `tool_result` 格式转换
7. **Web 管理后台**：内置管理面板，访问根路径 `/`（alpine.js，编译时嵌入二进制）
8. **配置热重载**：修改 `config.yaml` 后无需重启即可生效
9. **结构化日志**：应用日志通过 `slog` 输出 + 每次 LLM API 调用记录为 JSONL 格式
10. **HTTP 代理支持**：支持通过 HTTP 代理转发请求

### LLM API

| 路由 | 说明 |
|------|------|
| `/llm/<route-id>/v1/messages` | 转发到该路由配置的服务商 |
| `/llm/<route-id>/v1/messages/count_tokens` | Token 计数（Anthropic 直接代理，OpenAI/Google 估算） |

`route-id` 在 `config.yaml` 的 `routes` 下定义。每个路由有 `api_type`（`anthropic`、`openai`、`gemini`）和模型列表，模型可配置可选的回退 `targets`。

### 管理 API

| 路由 | 方法 | 说明 |
|------|------|------|
| `/api/config` | GET | 获取当前配置 |
| `/api/config` | POST | 更新配置（写入 `config.yaml` 并热重载） |
| `/api/routes` | GET | 列出所有路由 |
| `/api/providers` | GET | 列出所有服务商 |
| `/api/logs/llm` | GET | 查询 LLM 调用日志（`?date=YYYY-MM-DD&limit=100&offset=0`） |
| `/api/logs/app` | GET | 查看应用日志尾部（`?limit=100`） |

### 管理后台

在浏览器中打开 `http://localhost:8082/` 即可访问管理面板。

![admin ui](./docs/ui-1.png)

## 配置（`config.yaml`）

```yaml
app:
  level: info                     # 日志级别：debug/info/warn/error
  auth: true                     # 启用 API Key 认证
  listen: "0.0.0.0"              # 绑定地址
  port: "8082"                   # 监听端口（覆盖 PORT 环境变量）

users:
  - name: admin
    token: your_token            # 代理认证的 API Key
  - name: admin2
    password: your_password      # 替代凭证（同样有效）

tokens:
  - id: claude-code
    token: xxxxx                  # 额外的 API Key

routes:
  claude-code:
    api_type: anthropic
    targets:
      - name: openrouter
        enable: true
        models:
          - match_model: claude-opus*
            provider: openrouter
            model_id: anthropic/claude-opus-4-20250514
            api_name: default
          - match_model: claude-sonnet*
            provider: openrouter
            model_id: anthropic/claude-sonnet-4-20250514
            api_name: default
      - name: anthropic
        enable: true
        models:
          - match_model: '*'
            provider: anthropic
            model_id: claude-sonnet-4-20250514
            api_name: default

  codex:
    api_type: openai
    targets:
      - name: openrouter
        enable: true
        models:
          - match_model: plan
            provider: openrouter
            model_id: gpt-5.5
            api_name: default
          - match_model: plan
            provider: openrouter
            model_id: gpt-4o
            api_name: default

providers:
  openrouter:
    models:
      - model_id: openai/gpt-5.5
    apis:
      - name: default
        api_type: openai
        base_url: https://openrouter.ai/api/v1
        api_key: sk-or-xxx

  anthropic:
    models:
      - model_id: claude-sonnet-4-20250514
    apis:
      - name: default
        api_type: anthropic
        base_url: https://api.anthropic.com/v1
        api_key: sk-ant-xxx
```

### 配置字段说明

- `app.level`：应用日志级别（`debug`/`info`/`warn`/`error`）。覆盖 `LOG_LEVEL` 环境变量。
- `app.auth`：启用/禁用 API Key 认证。设为 `false` 时允许所有请求。
- `app.listen`：绑定地址（默认：`0.0.0.0`）。
- `app.port`：服务监听端口。覆盖 `PORT` 环境变量。
- `users`：API 访问用户列表。每个用户有 `name`，以及 `token` 或 `password` 之一（两者都被接受为有效 API Key）。
- `tokens`：额外的 API Key（以 `id` 标识）。
- `routes`：路由定义
  - `api_type`：`anthropic`、`openai` 或 `gemini`
  - `targets`：有序的目标组列表（故障切换链）。每组包含：
    - `name`：该目标组的标识
    - `enable`：是否启用该组
    - `models`：该组内的模型映射列表
      - `match_model`：匹配客户端发送的模型 ID 的模式。支持精确匹配、前缀通配符（`"prefix-*"`）和全通配符（`"*"`）。
      - `provider`：转发匹配请求到的服务商
      - `model_id`：实际发送给服务商的模型 ID
      - `api_name`：可选。选择服务商的特定 API 端点
- `providers`：服务商定义
  - `proxy`：可选的 HTTP 代理 URL，用于向该服务商发送请求（覆盖全局 `PROXY_URL` 环境变量）。
  - `models`：可用模型列表（用于文档/校验）。
  - `apis`：该服务商的 API 端点列表
    - `name`：该 API 端点的标识符
    - `api_type`：`openai`、`anthropic` 或 `gemini`
    - `base_url`：API 基础地址
    - `api_key`：API 密钥

## 环境变量

| 变量 | 必需 | 默认值 | 说明 |
|------|------|--------|------|
| `PORT` | 否 | `8082` | 服务监听端口。可被 `config.yaml` 中的 `app.port` 覆盖 |
| `LOG_LEVEL` | 否 | `info` | 日志级别（`debug`/`info`/`warn`/`error`）。可被 `config.yaml` 中的 `app.level` 覆盖 |

API Key 和基础地址现已配置在 `config.yaml` 中，不再使用环境变量。`.env` 文件仍支持 `PORT`、`LOG_LEVEL` 和 `PROXY_URL`。

## 使用方法

### 1. 创建 `config.yaml`

参见上方示例。最低要求：定义一个路由和一个服务商。若 `app.auth` 为 `true`（默认），至少配置一个用户或 token。

### 2. 运行服务

```bash
go run main.go
# 或指定自定义配置文件路径：
go run main.go /path/to/config.yaml
```

### 3. 发送请求

```bash
# 向路由发送请求（例如 config.yaml 中定义的 'claude-code'）
curl -X POST http://localhost:8082/llm/claude-code/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: your_token" \
  -d '{
    "model": "code",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

代理会查找路由 `claude-code`，找到模型 `code`，解析其目标列表，然后转发请求。若首个目标出现网络错误或 5xx，自动尝试下一个目标。

### 4. 通过 Web 后台管理

打开 `http://localhost:8082/`，使用 API Key 登录后即可浏览路由、服务商、日志，并编辑配置。

## 认证方式

代理服务支持两种认证方式：

1. **x-api-key 请求头**：`x-api-key: your_token`
2. **Authorization 请求头**：`Authorization: Bearer your_token`

Token 会与所有 `users` 的 token/password 以及所有 `tokens` 条目进行匹配。若 `app.auth` 设为 `false`，则禁用认证，接受所有请求。若认证启用但未配置任何用户或 token，则拒绝所有请求。

## 功能详情

### 格式转换

- **OpenAI/Gemini 路由**：将 Anthropic 格式请求转换为 OpenAI 格式，调用对应 API，再将响应转回 Anthropic 格式。
- **Anthropic 路由**：直接代理请求到 Anthropic API，不做格式转换。

### 流式响应

支持 SSE（Server-Sent Events）流式响应，包括：
- `message_start` — 消息开始
- `content_block_start` — 内容块开始
- `content_block_delta` — 内容增量
- `content_block_stop` — 内容块结束
- `message_delta` — 消息增量
- `message_stop` — 消息结束

### 工具调用

完整支持工具调用功能：
- 将 Anthropic 的 `tools` 格式转换为 OpenAI 的 `functions` 格式
- 处理 `tool_use` 和 `tool_result` 消息类型
- 自动清理 Google API 不支持的 schema 字段

### 日志

- **应用日志**：`logs/app.log` — 通过 `slog` 输出的结构化文本日志
- **LLM 调用日志**：`logs/llm-YYYY-MM-DD.jsonl` — 每次 API 调用一条 JSON 记录，包含字段：`timestamp`、`route_id`、`model_id`、`provider`、`target_model`、`duration_ms`、`status_code`、`error`、`input_tokens`、`output_tokens`

两种日志均可通过 Web 后台浏览。

## 依赖

- [github.com/google/uuid](https://github.com/google/uuid) — UUID 生成
- [github.com/joho/godotenv](https://github.com/joho/godotenv) — 环境变量加载
- [github.com/fsnotify/fsnotify](https://github.com/fsnotify/fsnotify) — 配置文件监听
- [gopkg.in/yaml.v3](https://gopkg.in/yaml.v3) — YAML 解析

## 安装依赖

```bash
go mod tidy
```

## Docker

### 构建镜像

```bash
docker build -t go-agents-proxy .
```

### 运行容器

```bash
docker run -d --name agents-proxy -p 8082:8082 \
    -v $(pwd)/config.yaml:/app/config.yaml \
    ghcr.io/mark0725/go-agents-proxy:latest
```

## 许可证

MIT
