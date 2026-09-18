# 清理 claude-code-router (ccr) 与 CC Switch 覆盖坑

面向「本机用 CC Switch / claude-code-router 之类的客户端管理器，把 Claude Code / Codex 接到
workbuddy2api」的场景。记录一次真实排查：**卸载 ccr 后配置反复"复活"**，根因是
第三方工具对 `~/.claude/settings.json` 采取**读-改-写 + 保留未知字段**策略。

> 本文是**本机客户端接入环境**的经验，与 workbuddy2api 服务本身无关。
> 服务端部署见 `deploy-nas.md`。

---

## 一、先看清几张配置的关系

这三个东西都可能在动同一批文件，**先分清谁管什么**，否则会像本文一样绕弯路。

| 组件 | 角色 | 动哪些文件 | 是否读 `OPENAI_*` / `ANTHROPIC_*` 环境变量 |
| --- | --- | --- | --- |
| **CC Switch** | GUI 管理多个 Provider，可在本地起代理转发 | **持久化改写** `~/.claude/settings.json`、`~/.codex/config.toml` | ❌ 不读，它自己写进文件里的 `env` 段 |
| **claude-code-router (ccr)** | 把 Anthropic 协议转成 OpenAI Chat | 也改写上述文件，并**寄生**多个目录 | ❌ 不读 |
| **本仓库 `deploy-nas.md` 第五节的 `wb-*` 函数** | 手动调用 / 给读 env 的 CLI 分流 | **不碰任何文件**，只设当前窗口环境变量 | ✅ 读 |

**关键区别**：

- `wb-*` 函数**只动当前 PowerShell 窗口的环境变量**，退出即失效，**不写配置文件** ——
  所以它跟 CC Switch / ccr **不会冲突**，可以放心共存。
- CC Switch 和 ccr **都在持久化改写同一批文件**，这才是冲突的来源。

```
本机 Claude Code ──> CC Switch 代理 (127.0.0.1:15721) ──> workbuddy2api (127.0.0.1:7865) ──> 上游
```

---

## 二、ccr 的"寄生"特征（为什么"卸载"不简单）

ccr **不是**通过 npm / 安装包装进去的 —— 它没有 uninstaller，而是通过**改写 agent 配置文件**
+ 在多个目录留下自己的痕迹来工作。所以"卸载 ccr" ≠ 删一个目录，而是
**删文件 + 把被它改过的配置恢复干净**。只删目录不修配置，Claude Code / Codex 会直接报错。

### 它留下的东西（实测清单）

**目录**（可能有**多份**，注意 Windows 上 Roaming 与 Local 各有一份、内容还不同）：

```
%APPDATA%\claude-code-router\
%LOCALAPPDATA%\claude-code-router\
~\.codex\.claude-code-router\
```

**残留文件**（形如 `*.ccr-*`，实测一次清理出 16 个）：

```
~\.claude\settings.json.ccr-original                    ← ccr 动手前的原始配置
~\.claude\settings.json.ccr-backup-2026-09-15T*.json    ← 每次改写前的备份 ×N
~\.claude\.claude.json.ccr-original-missing             ← 0 字节占位标记
~\.codex\config.toml.ccr-backup-2026-09-15T*.toml       ← 同上 ×N
~\.codex\config.toml.ccr-original
~\.codex\claude-code-router.config.toml
~\.codex\ccr-model-catalog.json.ccr-original-missing
```

**技能**（如果装过 AI 助手，可能还有配套的修复技能，删 ccr 后就没用了）：

```
~\.workbuddy-ai\skills\ccr-v3-config-repair\
```

> `*-original-missing` 是 ccr 的标记：表示"原始文件不存在"。它是**空文件**，
> 但会被 ccr 当作状态读取，**留着会干扰**后续工具，应当删掉。

### 它往配置里塞了什么（这才是要修的）

`~/.claude/settings.json` 里被塞进 5 处：

| 字段 | ccr 写的值 | 为什么必须清 |
| --- | --- | --- |
| `apiKeyHelper` | 指向 `%APPDATA%\claude-code-router\bin\ccr-...cmd` | **指向已删除文件**，不清则 Claude Code 启动即报错 |
| `env.ANTHROPIC_API_BASE_URL` | `http://127.0.0.1:3456` | ccr 的死端口 |
| `env.CLAUDE_AGENT_API_BASE_URL` | `http://127.0.0.1:3456` | 同上 |
| `env.ANTHROPIC_AUTH_TOKEN` | `PROXY_MANAGED` | ccr 的占位符 |
| `model` | `anthropic/claude-ccr-<hex>` | ccr 生成的虚拟模型名（hex 解码后是 `workbuddy/global:<模型名>`） |

`~/.codex/config.toml` 里主要是这些：

```
model_provider = "claude-code-router"
model = "workbuddy/global:deepseek-v4.1-flash"
```

> 那个 `claude-ccr-<hex>` 模型名容易让人困惑。用一行 Python 就能解出真身：
> ```python
> print(bytes.fromhex('776f726b62756464792f676c6f62616c3a646565707365656b2d76342e312d666c617368').decode())
> # workbuddy/global:deepseek-v4.1-flash
> ```

---

## 三、⚠️ 核心坑：CC Switch 会把 ccr 的痕迹"救回来"

**这是本次反复的根因，也是本文最重要的一条。**

CC Switch 写 `settings.json` 时是 **读-改-写**：它只替换**自己管理的那几项**
（`ANTHROPIC_BASE_URL` 等），对文件里**它不认识的字段**（`apiKeyHelper`、
`ANTHROPIC_API_BASE_URL`、`CLAUDE_AGENT_API_BASE_URL`）**原样保留并写回**。

于是形成死循环：

```
手动清掉 ccr 痕迹  →  在 CC Switch 里点「应用 / 切换」
                   →  CC Switch 把文件里残留的 ccr 字段保留着写回
                   →  痕迹"复活"
```

**症状**：明明删干净了，点一下 CC Switch 又冒出来，看起来像"删不掉"。

**正确顺序（务必按此）**：

1. **先**手工把 `~/.claude/settings.json` 修成干净状态
2. **再**删掉所有 `*.ccr-*` 残留文件（断了"保留源"）
3. 之后如需 CC Switch 重新接管，**这次**它读到的才是干净文件

> 反过来说：只做第 2 步、不做第 1 步，然后去点 CC Switch，等于白干。

### 另一个隐蔽点：CC Switch 的 DB 可能是"对的"

排查时容易怀疑 CC Switch 的内部数据库，但实测**它的 DB 里存的是正确的直连配置**
（`ANTHROPIC_BASE_URL = http://localhost:7865/v1`），问题**不在数据、在写入行为**。
别急着去改它的数据库。

它的库位置（SQLite，可用 `sqlite3` 或 Python 读）：

```
~\.cc-switch\cc-switch.db        ← providers / proxy_config / settings 等表
```

查看当前生效的 Claude Provider：

```python
import sqlite3, json
c = sqlite3.connect(r'C:\Users\<你>\.cc-switch\cc-switch.db')
c.row_factory = sqlite3.Row
for r in c.execute("SELECT id,app_type,name,settings_config FROM providers WHERE is_current=1"):
    if r['app_type'] == 'claude':
        print(r['name']); print(json.dumps(json.loads(r['settings_config']), ensure_ascii=False, indent=2))
```

> ⚠️ 这个库里**明文存着你的 API Key**，不要把它拷来拷去、更不要提交进任何仓库。

---

## 四、清理步骤（可直接照做）

### 1. 确认 ccr 没在跑

```powershell
Get-Process | Where-Object { $_.ProcessName -match 'ccr' }
netstat -ano | Select-String ":3456"      # 无输出 = 没监听
```

### 2. 先修配置文件（顺序不能反，见第三节）

备份 → 清掉 ccr 字段 → 保留 CC Switch 自己的项：

```powershell
$p = "$env:USERPROFILE\.claude\settings.json"
Copy-Item $p "$p.bak" -Force
```

用 Python 精确清理（**只删 ccr 的，保留 CC Switch 的 `ANTHROPIC_BASE_URL`**）：

```python
import json, io, collections
p = r'C:\Users\<你>\.claude\settings.json'
d = json.load(io.open(p, encoding='utf-8-sig'), object_pairs_hook=collections.OrderedDict)

d.pop('apiKeyHelper', None)                     # 指向已删除的 ccr bin
env = d.get('env', {})
for k in ('ANTHROPIC_API_BASE_URL', 'CLAUDE_AGENT_API_BASE_URL'):
    if env.get(k) == 'http://127.0.0.1:3456':   # ccr 的死端口
        del env[k]
if env.get('ANTHROPIC_AUTH_TOKEN') == 'PROXY_MANAGED':
    del env['ANTHROPIC_AUTH_TOKEN']
if isinstance(d.get('model'), str) and 'claude-ccr-' in d['model']:
    d['model'] = 'deepseek-v4.1-flash'          # 换成真实模型名

io.open(p, 'w', encoding='utf-8').write(json.dumps(d, ensure_ascii=False, indent=2) + '\n')
```

保留下来应该是这个形状（`ANTHROPIC_BASE_URL` 指向 CC Switch 代理）：

```json
{
  "env": { "ANTHROPIC_BASE_URL": "http://127.0.0.1:15721", "...": "模型映射保持原样" },
  "model": "deepseek-v4.1-flash",
  "theme": "dark"
}
```

> **注意**：CC Switch 的本地代理**不要求客户端带 token**（它自己持有真凭证），
> 所以删掉 `ANTHROPIC_AUTH_TOKEN` 是安全的。

### 3. 删所有 ccr 残留

```powershell
$targets = @(
  "$env:APPDATA\claude-code-router",
  "$env:LOCALAPPDATA\claude-code-router",
  "$env:USERPROFILE\.codex\.claude-code-router"
)
foreach ($t in $targets) { if (Test-Path $t) { Remove-Item $t -Recurse -Force; "DELETED $t" } }

# 清 *.ccr-* 残留文件
foreach ($dir in @("$env:USERPROFILE\.claude", "$env:USERPROFILE\.codex")) {
  Get-ChildItem $dir -Filter "*ccr-*" -File -ErrorAction SilentlyContinue |
    ForEach-Object { Remove-Item $_.FullName -Force; "DELETED $($_.Name)" }
}

# 配套技能（如有）
$sk = "$env:USERPROFILE\.workbuddy-ai\skills\ccr-v3-config-repair"
if (Test-Path $sk) { Remove-Item $sk -Recurse -Force; "DELETED skill" }
```

> 删 `*.ccr-*` 时注意：**别把你自己刚做的备份也匹配进去**。
> 上面按文件名过滤 `*ccr-*`，若你的备份名里含 `ccr` 会被一起删，先确认或改名。

### 4. 验证

```powershell
# a) 配置里不该再有 ccr 痕迹
$s = Get-Content "$env:USERPROFILE\.claude\settings.json" -Raw
'ccr', '3456', 'apiKeyHelper', 'PROXY_MANAGED' | ForEach-Object { "$_ = " + $s.Contains($_) }

# b) 目录与进程
Test-Path "$env:APPDATA\claude-code-router"          # False
Get-Process | Where-Object { $_.ProcessName -match 'ccr' }   # 空

# c) 端到端：经 CC Switch 代理打一次真实请求
curl.exe -s -X POST http://127.0.0.1:15721/v1/messages `
  -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" `
  -d '{\"model\":\"deepseek-v4.1-flash\",\"max_tokens\":30,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: OK\"}]}'
```

期望输出（说明整条链路通）：

```json
{"id":"cmb-...","type":"message","role":"assistant",
 "content":[{"type":"text","text":"OK"}],"model":"deepseek-v4.1-flash", ...}
```

---

## 五、排错速查

| 现象 | 去看 |
| --- | --- |
| 删了 ccr，一点 CC Switch 又回来 | 第三节（读-改-写保留策略，**先修文件再点应用**） |
| Claude Code 启动报 `apiKeyHelper` / 找不到文件 | 第二节表格（`apiKeyHelper` 指向已删的 ccr bin） |
| 请求打到 `127.0.0.1:3456` 失败 | ccr 死端口；3456 无监听，应改指 CC Switch 的 15721 或 7865 |
| 模型名 `anthropic/claude-ccr-<hex>` 看不懂 | 第二节末尾的 hex 解码 |
| 配置指向一个没人监听的端口 | `netstat -ano \| Select-String ":786[0-9]"`；有 `LISTENING` 才算活 |
| Claude Code 连不上但服务在跑 | 先 `Get-Content $env:USERPROFILE\.claude\settings.json -Raw` 看 `ANTHROPIC_BASE_URL` |

---

## 六、安全提醒

- **`~/.cc-switch/cc-switch.db` 明文存 API Key**，`~/.claude/settings.json`、
  `~/.codex/config.toml` 同理。备份、迁移、分享排查结果时**务必先脱敏**。
- `%APPDATA%\claude-code-router\` 下也曾明文存 key（含 `config.sqlite` 与其他目录里的副本，
  两份内容还不同）。**删除前不要随手打包发人**。
- 本文所有命令都**不包含**真实密钥，示例里用 `http://127.0.0.1:...` 与
  `<占位>` 表示；照抄时请替换为你自己的值。
- 把 `curl` 的结果贴给人看之前，先确认其中没有 key。

---

## 七、一句话总结

> **ccr 是"寄生"式的，卸载要删文件 + 修配置；
> CC Switch 是"读-改-写 + 保留未知字段"的，所以必须"先修配置、再让它接管"。
> 顺序颠倒，ccr 就会反复复活。**
