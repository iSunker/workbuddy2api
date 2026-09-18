# WorkBuddy2API 使用手册

> 本手册讲**日常怎么用**：调用本机 / NAS 的 Claude、恢复历史对话、常见问题。
> 部署与更新见 [`deploy-nas.md`](./deploy-nas.md)；项目原理见 [`../README.md`](../README.md)。

---

## 一、两个网关实例

| 实例 | 地址 | 管理后台 | 账号池 | 用途 |
|---|---|---|---|---|
| **本机** | `http://127.0.0.1:7865` | `/admin` | 本机 `auths/` | 本机终端用 |
| **NAS** | `http://192.168.0.7:7865` | `/admin` | NAS `auths/` | 本机第二终端 / 其他设备用 |

**两个实例的 `auths/`、`data/`、`config.json` 完全独立**，账号互不重复，`api_key` 也各不相同。

> **判据**：`curl --noproxy '*' http://<地址>/healthz` 的 `accounts` 字段 —— 本机 5、NAS 1。

**为什么分两个**：单个账号池没有容错能力（上游偶发错误时无处可退），
且分池可以用不同账号并行、互不干扰配额。

---

## 二、调用 Claude —— 三种方式

### 方式 A：Claude Code 直连（推荐，日常用这个）

**前提**：网关配置里 `features.enable_anthropic_protocol = true`（本机与 NAS 都已开启）。

**第 1 步**：确认 profile 文件存在（`claude-profiles/` 目录，**含真实密钥，不入 git**）：

```
claude-profiles/wb-local.json   →  指向本机 127.0.0.1:7865
claude-profiles/wb-nas.json     →  指向 NAS  192.168.0.7:7865
```

若不存在，从模板复制并填 key：

```powershell
cd D:\Projects\workbuddy2api\claude-profiles
Copy-Item wb-local.example.json wb-local.json
Copy-Item wb-nas.example.json   wb-nas.json
# 然后编辑两个文件，把 <本机网关的 api_key> / <NAS 网关的 api_key> 换成真实 key
notepad wb-local.json
```

**第 2 步**：用 `--settings` 启动（**每个终端开一次**）：

```powershell
# 终端 A —— 本机账号池
claude --settings D:\Projects\workbuddy2api\claude-profiles\wb-local.json

# 终端 B —— NAS 账号池
claude --settings D:\Projects\workbuddy2api\claude-profiles\wb-nas.json
```

**两个终端各走各的账号池，互不干扰。** 界面顶部会显示实际用的模型
（应为 `deepseek-v4.1-flash`）。

> ### ⚠️ 必须用 `--settings`，不能用环境变量
>
> ```powershell
> # ❌ 这样无效！会被 ~/.claude/settings.json 里的同名字段覆盖
> $env:ANTHROPIC_BASE_URL = 'http://192.168.0.7:7865'
> claude
> ```
>
> 原因：`~/.claude/settings.json` 的 `env` 段**优先级高于进程环境变量**
> （Claude Code 自身设定，不是 OS 规则）。而那个文件被 CC Switch 托管，
> 写着 `ANTHROPIC_BASE_URL=http://127.0.0.1:15721`，于是请求被拉回 CCS。
>
> **`--settings` 是命令行参数，优先级高于该文件，所以能生效。**

### 方式 B：CC Switch 走第三方中转站

你 CCS 里有十几个第三方 provider（`air-outer.com`、`achai.cc`、`deepseek.com/anthropic` …）。
这些**网关替代不了**，必须走 CCS：

```powershell
# 不带 --settings，走 ~/.claude/settings.json → CCS:15721 → 你在 CCS GUI 里选的 provider
claude
```

**CCS 的当前 provider 是全局状态**（`providers.is_current`），不是 per-终端 ——
所以**不能用两个终端走 CCS 来区分实例**，那会互相打架。

### 方式 C：OpenAI 兼容客户端 / curl

任何支持 OpenAI Chat 的客户端（Cherry Studio、LobeChat、NextChat、Open WebUI…）：

| 项 | 值 |
|---|---|
| Base URL | `http://<实例地址>:7865/v1` |
| API Key | 该实例 `config.json` 里的 `api_key` |
| 模型名 | `/v1/models` 返回的 `id`，如 `deepseek-v4.1-flash` |

```bash
# curl 实测（--noproxy '*' 绕开本机系统代理）
curl --noproxy '*' http://192.168.0.7:7865/v1/chat/completions \
  -H "Authorization: Bearer <NAS 的 api_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"你好"}]}'
```

---

## 三、恢复历史对话

Claude Code 的会话**按项目目录隔离**存在 `~/.claude/projects/<项目路径转义>/<session-id>.jsonl`。

### 三种恢复方式

| 命令 | 作用 | 适用场景 |
|---|---|---|
| `claude -c` | 继续**当前目录**最近一次对话 | 最常用 |
| `claude --resume` | 弹出**交互列表**让你选 | 想挑某个历史会话 |
| `claude --resume <session-id>` | 直接恢复指定会话 | 已知 id |

### ★ 关键：恢复时必须带 `--settings`，否则会串池

```powershell
# ✅ 正确 —— 用本机池继续本机目录的最近对话
claude --settings D:\Projects\workbuddy2api\claude-profiles\wb-local.json -c

# ✅ 正确 —— 用 NAS 池继续
claude --settings D:\Projects\workbuddy2api\claude-profiles\wb-nas.json -c

# ❌ 危险 —— 不带 --settings 会走 CCS，把对话发到第三方中转站
claude -c
```

> **为什么危险**：会话内容是**跨实例共享的**（都存在本机 `~/.claude/projects/`），
> 但**请求发到哪个网关由启动参数决定**。不带 `--settings` 时走 CCS，
> 于是「恢复 NAS 的对话」实际会把上下文发给 CCS 里选中的第三方 provider。

### 恢复 + 分叉（想保留原会话时用）

```powershell
# 恢复但新建一个 session id，原会话不动
claude --settings ...\wb-nas.json --resume <session-id> --fork-session
```

### 选会话：怎么拿到 session-id

```powershell
# 列出某项目的所有会话（按修改时间倒序，最新在最下）
ls -lt ~/.claude/projects/D--Projects-BlogDevelop-my-blog/*.jsonl | head

# 或直接让 Claude Code 弹列表选
claude --settings ...\wb-nas.json --resume
```

**项目目录的转义规则**：路径里的 `:` `\` `/` 等替换成 `-`
（如 `D:\Projects\BlogDevelop\my-blog` → `D--Projects-BlogDevelop-my-blog`）。

---

## 四、管理后台

浏览器打开 `http://<实例地址>:7865/admin`：

| 功能 | 说明 |
|---|---|
| 顶部统计 | 账号总数 / 可用 / 冷却中 / 已停用 / 积分剩余 |
| 账号列表 | 逐账号健康度、额度、今日已签/未签；可测试 / 停用 / 删除 |
| 自动授权登录 | 一键 OAuth 绑号（中国版 / 国际版） |
| 一键签到 | 对池内 OAuth 账号逐个官方签到 |
| 模型 / 对话测试 | 流式、非流式对话测试 |
| 客户端接入 | 一键复制 Base URL / API Key |

> ⚠️ **管理页会把真实 key 注入前端**（`window.__API_KEY__`），
> 只在内网开放，**不要映射到公网**。

---

## 五、用 curl 排查（--noproxy 很关键）

本机 Git Bash 的 `curl` 会读系统代理（`192.168.0.7:7894`），
访问本机/NAS 时**必须加 `--noproxy '*'`**，否则会绕远或失败：

```bash
K_L=http://127.0.0.1:7865; K_N=http://192.168.0.7:7865
A_L=<本机 api_key>;        A_N=<NAS api_key>

# 健康检查（看 accounts / healthy）
curl -s --noproxy '*' $K_L/healthz
curl -s --noproxy '*' $K_N/healthz

# 账号明细（含 realm / 是否停用 / 冷却原因）
curl -s --noproxy '*' -H "Authorization: Bearer $A_N" $K_N/api/status

# 最近消费明细（判断请求打到哪台）
curl -s --noproxy '*' -H "Authorization: Bearer $A_N" "$K_N/api/usage/records?limit=5"
```

**判断「某个终端在用哪个实例」的权威方法**：发一句话，然后查两台的
`/api/status` 里账号的 `last_used` 时间戳，哪台变了就是哪台。

---

## 六、常见问题

### Q1：Claude Code 报 `The model's tool call could not be parsed`

**已在 2026-09-19 修复**（网关 `content_block_*` 事件漏发 SSE 顶层 `type`）。
若再出现，先确认网关镜像是最新的：

```bash
grep -c '"content_block_start"' internal/upstream/anthropic.go   # 应为 6
```

仍报错则查日志：

```bash
sudo docker logs --tail 100 workbuddy2api-cb2api-1 | grep -E "toolSchemas|conformToolInput|stream tool_use|fallback"
```

### Q2：界面顶部显示 `hy4-preview` 而不是 `deepseek-v4.1-flash`

说明**模型映射没生效**，Claude Code 发的是 `claude-opus-5`，网关不认识就回落了。

检查 profile 里是否有这 10 个变量（重点 `ANTHROPIC_MODEL`）；改完要**新开终端**
（profile 在进程启动时读取，不会热加载）。

### Q3：`~/.claude/settings.json` 的改动被「复活」了

该文件被 **CC Switch 托管**（`enableLocalProxy: true`），你在 CCS GUI 里切
provider 时它可能把 `apiKeyHelper` 等写回来。确认现状：

```powershell
Get-Content "$env:USERPROFILE\.claude\settings.json" -Raw
```

备份在 `settings.json.bak-<时间戳>`，需要时 `Copy-Item` 覆盖回去。

### Q4：NAS 上怎么更新代码

NAS 的 22 端口从本机不一定通。**用 wget 反向拉**（详见 `deploy-nas.md` 第二节方式三）：

```bash
# 本机 Git Bash：起临时 HTTP 服务
cd /tmp && python3 -m http.server 8899

# NAS（Termius）：拉取 + 解包 + 重建
cd /volume2/docker_ssd/workbuddy2api
wget http://192.168.0.56:8899/wb2api-src.tar.gz
tar xzf wb2api-src.tar.gz && rm wb2api-src.tar.gz
chmod 755 . && chmod -R a+r .
sudo docker compose -f docker-compose.nas.yml up -d --build
```

### Q5：想新增账号

两种方式（两边实例**分别**操作）：

1. **管理后台** →「自动授权登录」→ 选中国版 / 国际版（推荐，可参与每日签到）
2. **`ck_` Key**：`./addkey.sh ck_xxx 备注名`（脚本要真 key，别照抄占位符）

改完在后台点「重载目录」，或重启容器。

> ⚠️ **不要在两个实例上用同一个账号/凭证**并发跑，容易触发上游风控。

---

## 七、日常命令速查

```powershell
# ===== Claude Code =====
claude --settings ...\wb-local.json            # 本机池，新对话
claude --settings ...\wb-nas.json              # NAS 池，新对话
claude --settings ...\wb-nas.json -c           # NAS 池，继续最近对话
claude --settings ...\wb-nas.json --resume     # NAS 池，选历史会话
claude                                          # 走 CCS（第三方中转站）

# ===== 网关状态 =====
curl -s --noproxy '*' http://127.0.0.1:7865/healthz   # 本机
curl -s --noproxy '*' http://192.168.0.7:7865/healthz # NAS
```

```bash
# ===== NAS 运维（Termius）=====
sudo docker logs --tail 50 workbuddy2api-cb2api-1     # 日志
sudo docker logs -f workbuddy2api-cb2api-1            # 实时日志
sudo docker compose -f docker-compose.nas.yml restart # 重启
sudo docker compose -f docker-compose.nas.yml ps      # 状态
```

---

## 八、一条重要的纪律

**`claude-profiles/` 里含真实网关密钥，已在 `.gitignore` 中。**
仓库里只有 `*.example.json` 模板。如果你新建 profile，
**不要 `git add -f`**，否则密钥会进入 git 历史。
