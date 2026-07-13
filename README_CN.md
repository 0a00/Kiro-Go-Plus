# Kiro-Go Plus

[![Test](https://github.com/0a00/Kiro-Go-Plus/actions/workflows/test.yml/badge.svg)](https://github.com/0a00/Kiro-Go-Plus/actions/workflows/test.yml)
[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Compose-2496ED?logo=docker)](https://docs.docker.com/compose/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

面向多账号场景的 Kiro API 网关，提供 OpenAI、Anthropic 和 Responses API 兼容接口，以及可在 Web 管理面板完成配置的账号池、缓存、刷新、代理、监控和安全能力。

[English](README.md) | 中文

> 本项目是非官方社区增强版本，与 Amazon、AWS 或 Kiro 官方无隶属、授权或背书关系。请自行确认使用方式符合服务条款和当地法律。

## 项目定位

Kiro-Go Plus 保留原 Kiro-Go 的接口兼容性和部署方式，重点增强生产环境中的稳定性与可运维性：

- 协议兼容：Anthropic `/v1/messages`、OpenAI `/v1/chat/completions`、OpenAI `/v1/responses`、`/v1/models`
- 上游通道：Kiro Runtime 主通道，支持旧 Kiro / CodeWhisperer / Amazon Q 端点回退
- 多账号调度：加权、优先级、均衡三种模式，账号级并发限制、粘性会话和失败切换
- 刷新体系：Token 刷新去重、并发队列、超时、抖动、自适应批量刷新，适合几十到数百账号
- 故障保护：首包超时、有效输出与必需工具调用校验、流中断检测、端点熔断、账号冷却和有界重试
- 流式解析：AWS EventStream 长度与 CRC 校验、空闲超时、截断响应检测
- 认证方式：Builder ID、IAM Identity Center、Kiro 托管 SSO、Microsoft 365 / Entra ID、SSO Token、API Key 和 JSON 导入
- Prompt Cache：可设置缓存创建与读取比例区间、5m/1h TTL、分片 LRU、API Key 隔离和统计
- 扩展能力：动态模型发现、Web Search、外部 Token 计数、Responses 历史存储
- 运维能力：请求日志（含 API Key、停止原因、可见/思考输出与工具调用数）、诊断事件、Webhook 告警、`/health`、`/ready`、运行状态持久化
- Token 与 Agent 稳定性：可配置默认思考、最大输出和上下文预算；客户端显式值优先，工具流在确认有效输出前允许自动回退，确认后实时转发
- 出站网络：全局和账号级 HTTP / SOCKS5 代理

Prompt Cache 仅模拟并统计 Anthropic 缓存用量，不缓存模型响应正文。

Token 预算优先级为：请求显式参数、模型专属配置、Web 全局默认、模型自动识别。按协议支持 `max_tokens`、`max_completion_tokens`、`max_output_tokens`、`context_window` 和 `max_input_tokens` 等覆盖参数。

## Web 管理

访问 `/admin` 后可以管理：

- 账号添加、批量导入、启停、权重、优先级、单账号并发和独立代理
- Runtime / 旧端点选择及自动回退
- 负载均衡、重试、超时、熔断和上游保护
- Token 与模型刷新周期、刷新并发和批量大小
- Prompt Cache 创建比例、读取比例、TTL、容量和隔离方式
- Web Search、Token 计数、Responses 存储、诊断和告警
- Claude Agent 工具调用强制策略、思考/输出/上下文 Token 默认值、响应格式和缓冲校验
- API Key、配额、管理密码、监听地址和客户端指纹

除监听地址等需要进程重启的项目外，设置保存后会立即生效。

## 快速部署

### 1. 克隆并准备配置

```bash
git clone https://github.com/0a00/Kiro-Go-Plus.git
cd Kiro-Go-Plus
mkdir -p data
cp .env.example .env
```

生成主密钥：

```bash
openssl rand -base64 32
```

编辑 `.env`，至少设置：

```dotenv
ADMIN_PASSWORD=
KIRO_MASTER_KEY=
KIRO_PORT=8080
PUID=1000
PGID=1000
```

`KIRO_MASTER_KEY` 用于加密账号凭据和可选的 Responses 历史。该密钥必须长期保持不变，丢失后无法解密已有数据。

### 2. 启动

```bash
docker compose config
docker compose up -d --build
docker compose ps
```

管理面板：`http://127.0.0.1:8080/admin`

健康检查：

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/ready
```

### 3. 配置 API Key

Compose 默认监听容器的 `0.0.0.0:8080`。为避免公网匿名访问，兼容 API 默认采用失败关闭策略。请在 Web 管理面板创建并启用 API Key，再通过反向代理提供 TLS。

## API 示例

先把管理面板创建的 API Key 放入当前终端环境，并将模型名称替换为实际可用值：

```bash
export KIRO_API_KEY='在本机填写，不要写入仓库'
```

```bash
curl http://127.0.0.1:8080/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -H "x-api-key: ${KIRO_API_KEY}" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'
```

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${KIRO_API_KEY}" \
  -d '{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"你好"}]}'
```

## Microsoft 365 / Entra ID SSO

Kiro 托管 SSO 使用固定回调地址 `http://localhost:3128`。Compose 只在宿主机回环地址发布该端口。

如果管理面板运行在远程服务器，请先在本机执行：

```bash
ssh -L 3128:127.0.0.1:3128 user@server
```

然后在本机浏览器打开管理面板并开始登录。同一实例一次只能进行一个托管 SSO 登录。登录后会自动探测 Kiro Profile；存在多个 Profile 时可在 Web 中选择和切换。

可通过以下变量调整 Profile 探测区域：

```dotenv
KIRO_PROFILE_REGIONS=us-east-1,eu-central-1
```

## 更新已有 Docker Compose 部署

在新版本目录中执行：

```bash
./scripts/update-docker-compose.sh --target /path/to/old/project --yes
```

更新脚本会：

- 保留旧目录的 `data/`、`data/config.json`、运行状态和 `.env*`
- 在 `.update-backups/` 创建可回滚备份
- 校验 Compose、重建镜像、启动容器并执行健康检查
- 构建或健康检查失败时自动恢复旧版本

自定义 Compose 文件时使用 `--keep-compose`；需要把账号池状态纳入检查时使用 `--readiness-path /ready`。

## 源码运行

```bash
go test ./...
go build -o kiro-go .
./kiro-go
```

项目显示名称已改为 Kiro-Go Plus，但 Go module、二进制名、Compose 服务名和数据结构继续保留 `kiro-go`，用于兼容旧部署和更新脚本。

## 数据与安全

以下内容不得提交或公开：

- `.env`、`.env.*`
- `data/` 和 `data/config.json`
- `kiro-accounts-*.json`、账号或凭据导出文件
- 私钥、证书私钥、数据库、日志和备份文件

仓库已通过 `.gitignore` 与 `.dockerignore` 排除这些路径。发布前仍应执行：

```bash
git status --ignored
git diff --check
```

生产建议：

- 使用随机 `ADMIN_PASSWORD` 和固定的 `KIRO_MASTER_KEY`
- 启用 API Key 校验，不要在公网设置 `ALLOW_UNAUTHENTICATED_API=true`
- 使用 HTTPS 反向代理，并限制 `/admin` 的访问来源
- 为每个账号配置稳定的出站网络；账号级代理故障时按配置决定是否允许直连回退
- 定期备份 `data/`，并将主密钥保存在独立的安全位置

## 健康检查

- `GET /health`：进程存活即返回 200，适合作为容器 liveness
- `GET /ready`：可用账号数量或比例低于阈值时返回 503，适合作为负载均衡 readiness

Docker Compose 使用 `/health`，账号耗尽不会导致容器反复重启。反向代理或负载均衡器应使用 `/ready` 决定是否继续分发请求。

## 环境变量

| 变量 | 说明 | 默认值 |
|---|---|---|
| `CONFIG_PATH` | 配置文件路径 | `data/config.json` |
| `ADMIN_PASSWORD` | Web 管理密码，覆盖配置文件 | - |
| `LOG_LEVEL` | `debug`、`info`、`warn`、`error` | `info` |
| `KIRO_PORT` | Compose 发布到宿主机的端口 | `8080` |
| `KIRO_LISTEN_HOST` / `KIRO_LISTEN_PORT` | 进程监听地址；Compose 固定容器端为 `0.0.0.0:8080` | 配置值 |
| `PUID` / `PGID` | 容器非 root 用户，应与宿主机 `data/` 所有者一致 | `1000` |
| `KIRO_MASTER_KEY` | 32 字节 Base64 或 Hex 主密钥 | - |
| `KIRO_MASTER_KEY_FILE` | 从 secret 文件读取主密钥，优先于环境变量 | - |
| `ALLOW_INSECURE_PUBLIC_BIND` | 允许公网监听时使用默认管理密码，仅用于紧急排障 | `false` |
| `ALLOW_UNAUTHENTICATED_API` | 显式允许公网匿名调用兼容 API | `false` |
| `KIRO_SSO_CALLBACK_BIND` | 托管 SSO 回调监听地址 | 仅回环地址 |
| `KIRO_PROFILE_REGIONS` | Entra ID Profile 的逗号分隔探测区域 | `us-east-1,eu-central-1` |

## 上游与致谢

本项目基于 [Quorinex/Kiro-Go](https://github.com/Quorinex/Kiro-Go) 继续开发，并参考、适配了以下项目中的实现和思路：

- [zsecducna/Kiro-Go](https://github.com/zsecducna/Kiro-Go)
- [zsecducna/kiro-login-helper](https://github.com/zsecducna/kiro-login-helper)
- [Zhang161215/kiro.rs](https://github.com/Zhang161215/kiro.rs)

感谢原作者和相关贡献者。原项目许可证与版权声明保留在 [LICENSE](LICENSE) 中。

## 免责声明

本项目仅用于学习、研究和经授权的接口集成。不得用于绕过访问控制、配额、计费、服务限制或其他安全机制。使用者需自行承担账号、数据、合规和服务可用性风险。

## License

[MIT](LICENSE)
