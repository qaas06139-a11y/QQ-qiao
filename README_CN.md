# Kiro-Go

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

将 Kiro 账号转换为 OpenAI / Anthropic 兼容的 API 服务。

[English](README.md) | 中文

如果这个项目帮到了你，欢迎点个 Star 支持一下。

## 功能特性

- Anthropic `/v1/messages`、OpenAI `/v1/chat/completions` 与 OpenAI `/v1/responses`（Codex 兼容）
- 多账号池轮询负载均衡
- 自动 Token 刷新、SSE 流式输出、Web 管理面板
- 多种认证方式：AWS Builder ID、IAM Identity Center (企业 SSO)、SSO Token、本地缓存、凭证 JSON
- 用量追踪、账号导入导出、中英双语
- 支持设置出站代理（SOCKS5 / HTTP）

## 快速开始

### Docker Compose（推荐）

```bash
git clone https://github.com/Quorinex/Kiro-Go.git
cd Kiro-Go
mkdir -p data
docker-compose up -d
```

### Docker 运行

```bash
docker run -d \
  --name kiro-go \
  -p 8080:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/quorinex/kiro-go:latest
```

### 源码编译

```bash
git clone https://github.com/Quorinex/Kiro-Go.git
cd Kiro-Go
go build -o kiro-go .
./kiro-go
```

首次运行会在 `data/config.json` 自动生成配置，挂载 `/app/data` 以持久化。默认管理密码为 `changeme`，生产环境请务必通过 `ADMIN_PASSWORD` 环境变量或在管理面板中修改。

## 使用方法

访问 `http://localhost:8080/admin` 登录、添加账号，然后调用 API：

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"你好！"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"你好！"}]}'
```

## 配合 Codex CLI 使用

Kiro-Go 的 `/v1/responses` 端点兼容 OpenAI Responses API，可作为 Codex CLI 的自定义 provider。在 `~/.codex/config.toml` 中添加：

```toml
model_provider = "custom"
model = "claude-sonnet-4.5"

[model_providers.custom]
name = "custom"
base_url = "http://localhost:8080/v1"
wire_api = "responses"      # 也可以用 "chat"
requires_openai_auth = true
```

`~/.codex/auth.json`：

```json
{ "OPENAI_API_KEY": "your-api-key" }
```

## 思考模式

在模型名后加后缀（默认 `-thinking`）即可启用，例如 `claude-sonnet-4.5-thinking`。Claude 兼容请求如果带有顶层 `thinking` 配置，例如 `{"type":"enabled","budget_tokens":2048}` 或 `{"type":"adaptive"}`，也会自动启用 thinking 模式。输出格式可在管理面板「设置 - Thinking 模式」中配置。

## 出站代理

可在管理面板「设置 - 出站代理设置」中配置代理。支持 SOCKS5 和 HTTP 代理。

设置保存后即时生效，无需重启服务。

## 限流

公网部署建议在管理面板「设置 - 速率限制」中开启限流：

- **每分钟请求数** (RPM)：每个客户端每分钟最多处理多少次请求
- **突发容量** (Burst)：允许短时间内并发的请求数（token bucket capacity）

客户端按 API Key 优先、退化为 IP 区分。超出限流时返回 `HTTP 429` + `Retry-After` 头。`/admin/*` 与 `/health` 不受限流影响。

也可直接通过 admin API 配置：

```bash
curl -X POST http://localhost:8080/admin/api/rate-limit \
  -H "X-Admin-Password: your_admin_password" \
  -H "Content-Type: application/json" \
  -d '{"enabled":true,"requestsPerMinute":60,"burst":30}'
```

## 环境变量

| 变量 | 说明 | 默认值 |
|-----|------|-------|
| `CONFIG_PATH` | 配置文件路径 | `data/config.json` |
| `ADMIN_PASSWORD` | 管理面板密码（覆盖配置文件） | - |

## 参与贡献

欢迎友好交流。遇到问题时，建议先让 Claude Code、Codex 等工具帮忙排查一下，大部分问题都能自己解决。如果能直接提个 PR 就更好了。

## 友情链接

- [LINUX DO](https://linux.do)

## 免责声明

本项目仅供学习和研究目的使用，与 Amazon、AWS 或 Kiro 没有任何关联。用户需自行确保使用行为符合所有适用的服务条款和法律法规，使用风险自负。

## 许可证

[MIT](LICENSE)
