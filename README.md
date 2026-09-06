# CodeBuddy2API

> CodeBuddy 官方 API 的 OpenAI 兼容反向代理：多凭证轮转、自动冷却、流式 / 非流式双支持、Web 管理后台。

架构参考 [qoderwork2api](https://github.com/Sliverkiss/qoderwork2api)（账号池 + 冷却状态机 + 调度器 + OpenAI 兼容层），
上游替换为腾讯 CodeBuddy 的官方 OpenAI 兼容端点。

## 与 QoderWork2API 的差异

| 维度 | QoderWork2API | CodeBuddy2API |
|---|---|---|
| 上游 | `gateway.qoder.com.cn` 私有协议 | `copilot.tencent.com` **OpenAI 兼容** |
| 认证 | OAuth 设备授权 `dt-/drt-` | `ck_` API Key（或 OAuth Bearer Token） |
| 请求签名 | COSY（RSA + AES-CBC + MD5） | 无签名，`Bearer` / `X-Api-Key` 即可 |
| Body 编码 | QoderEncoding（自定义 base64 重排） | 无，标准 JSON 透传 |
| 响应 | 嵌套 SSE（`data:{"body":"<json>"}`） | 标准 OpenAI SSE |
| 非流式 | 聚合嵌套 SSE | 聚合标准 SSE（**上游只支持流式**） |
| 选号 | 按积分降序 | 按「错误数升序 → 最久未用优先」轮转 |

因为上游本身就是 OpenAI 格式，转发层几乎是纯透传，只做了两处必要适配：
1. **强制 `stream=true`** — 上游不接受非流式请求（会报 `Non-stream chat request is currently not supported`），非流式由本服务聚合后再返回；
2. **清洗 tools** — 丢弃非 `function` 类型的工具、空 `parameters`，并移除上游不支持的 `strict` / `additionalProperties` 字段。

## 功能特性

- 🔑 **多凭证轮转** — 多个 `ck_` Key 均匀分摊请求，单号故障自动切换
- 🧊 **冷却状态机** — 429 短冷却、配额耗尽长冷却、连续错误中冷却、凭证失效禁用，防雪崩
- 📡 **流式 + 非流式** — 上游恒流式，客户端要哪种都给哪种（`tool_calls` 按 index 正确合并）
- 🧩 **工具调用** — 完整支持 OpenAI `tools` / `tool_choice`，自动过滤上游不兼容的工具定义
- 🧠 **推理内容** — `reasoning_content` 单独透传
- 🗂 **动态模型列表** — 每小时从 `/v2/models` 或 `/v3/config` 拉取，失败回退静态表
- ⏰ **定时保活** — 按整点探测全部凭证，失效自动禁用
- 🖥 **Web 管理后台** — 增删凭证、查看冷却状态、在线对话测试
- 🐳 **Docker 一键部署** — 多阶段构建，healthcheck 常驻

## 快速开始

### 1. 获取 API Key

| 版本 | 获取地址 |
|---|---|
| 中国版 | https://copilot.tencent.com/profile/keys |
| 海外版 | https://www.codebuddy.ai/profile/keys |

Key 形如 `ck_xxxxxxxx`。

### 2. 配置

```bash
cp config.example.json config.json
# 编辑 config.json，至少改掉 api_key
```

### 3. 添加凭证

三种方式任选其一：

```bash
# 方式 A：脚本（Linux / macOS）
./addkey.sh ck_xxxx 我的账号

# 方式 B：脚本（Windows PowerShell）
.\addkey.ps1 -Key ck_xxxx -Name 我的账号

# 方式 C：直接放文件
# auths/codebuddy-我的账号.json
# {"api_key":"ck_xxxx","uid":"我的账号","nickname":"我的账号","type":"api_key"}
```

### 4. 启动

```bash
# Docker（推荐）
docker compose up -d --build

# 或者本机
go build -o cb2api ./cmd/server && ./cb2api -config config.json
```

后台地址：<http://localhost:7865/admin>

### 5. 验证

```bash
# 模型列表
curl -s http://localhost:7865/v1/models -H "Authorization: Bearer your-api-key"

# 非流式
curl -s http://localhost:7865/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}'

# 流式
curl -N http://localhost:7865/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

接入任意 OpenAI 客户端：Base URL 填 `http://localhost:7865/v1`，API Key 填配置的 `api_key`。

## 诊断工具

不确定 Key 是否可用、或者想知道这个 Key 能看到哪些模型时：

```bash
go run ./cmd/check -key ck_xxxx                 # 探测 + 列出模型
go run ./cmd/check -key ck_xxxx -model glm-5.2  # 再跑一次真实对话
go run ./cmd/check -key ck_xxxx -env public     # 海外版
```

## 配置说明

```jsonc
{
  "listen": ":7865",
  "api_key": "your-api-key-here",        // 客户端访问本服务用的 Key，留空 = 不鉴权
  "auth_dir": "./auths",                 // 凭证目录
  "state_file": "./data/state.json",     // 冷却/禁用状态持久化

  "upstream": {
    "base": "https://copilot.tencent.com", // 留空时按 env 推导
    "env": "internal",                     // internal | public | ioa
    "timeout_seconds": 180
  },

  "oauth": {                              // 仅 token 型凭证续期时需要
    "token_url": "",
    "client_id": ""
  },

  "cooldown": {
    "hard_credit": "12h",   // 配额/余额不足 → 长冷却
    "soft_rate": "60s",     // 429 / 404 → 短冷却
    "err_threshold": 5,     // 连续错误阈值
    "err_cooldown": "10m"   // 达阈值后冷却
  },

  "schedule": {
    "check_hours": [0, 6, 12, 18]         // 每天这些整点做一次凭证探活
  }
}
```

### 环境变量覆盖

| 变量 | 对应配置 |
|---|---|
| `CB2A_LISTEN` | `listen` |
| `CB2A_API_KEY` | `api_key` |
| `CB2A_AUTH_DIR` | `auth_dir` |
| `CB2A_STATE_FILE` | `state_file` |
| `CB2A_UPSTREAM_BASE` | `upstream.base` |
| `CB2A_UPSTREAM_ENV` | `upstream.env` |
| `CB2A_OAUTH_TOKEN_URL` | `oauth.token_url` |
| `CB2A_OAUTH_CLIENT_ID` | `oauth.client_id` |
| `CB2A_HARD_CREDIT` / `CB2A_SOFT_RATE` / `CB2A_ERR_COOLDOWN` | 冷却时长 |
| `CB2A_ERR_THRESHOLD` | 连续错误阈值 |
| `CB2A_TIMEOUT_SECONDS` | 上游超时（秒） |

## API

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/v1/chat/completions` | ✅ | OpenAI 兼容对话（stream 可选） |
| GET | `/v1/models` | ✅ | 模型列表 |
| GET | `/healthz` | ❌ | 健康检查（容器 healthcheck 用） |
| GET | `/api/status` | ✅ | 服务 + 账号总览 |
| GET | `/api/accounts` | ✅ | 账号列表 |
| POST | `/api/accounts` | ✅ | 添加凭证 `{api_key, uid?}` |
| POST | `/api/accounts/reload` | ✅ | 重新扫描 `auth_dir` |
| POST | `/api/accounts/enable?uid=` | ✅ | 解禁 / 解冻 |
| DELETE | `/api/accounts?uid=` | ✅ | 删除凭证（含文件） |
| GET | `/admin` | ❌ | Web 管理后台 |

## 项目结构

```
buddy2api/
├── cmd/
│   ├── server/           主服务
│   │   ├── main.go       装配 + 优雅退出
│   │   └── config.go     JSON 配置 + CB2A_* 环境变量
│   └── check/            凭证诊断 CLI
├── internal/
│   ├── cred/             凭证加载 / 保存 / 续期
│   ├── pool/             账号池 + 冷却状态机 + state.json
│   ├── scheduler/        定时探活
│   ├── upstream/         CodeBuddy API 客户端
│   │   ├── client.go     HTTP + 双认证头
│   │   ├── chat.go       请求体构造 + tools 清洗
│   │   ├── models.go     动态模型（宽松解析多种形态）
│   │   ├── sse.go        SSE 解析 / 聚合 / 透传
│   │   └── classify.go   错误分类 → 冷却决策
│   └── server/           OpenAI 兼容路由 + 管理 API + 后台页面
├── auths/                凭证（gitignore）
├── data/                 运行时状态（gitignore）
├── addkey.sh / addkey.ps1
├── Dockerfile / docker-compose.yml
└── config.example.json
```

## 开发

```bash
go build ./...
go test ./...
gofmt -l .
```

## 免责声明

本项目仅供学习研究使用。使用时请遵守 CodeBuddy 的服务条款，自行承担相应风险。作者不对因使用本项目产生的任何直接或间接损失负责。

## License

MIT
