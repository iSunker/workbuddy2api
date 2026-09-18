# 在 NAS 上部署（群晖 DSM / 威联通 / 通用 Docker）

面向「把本网关跑在家里 NAS 上，仅内网访问」的场景。生产服务器（icebears.cn + nginx 反代）的部署方式见
`docker-compose.server.yml` 与仓库根目录的 `deploy.sh`，两者不通用，**不要混用**。

- `docker-compose.yml` — 本地开发：写死 `container_name: codebuddy2api`
- `docker-compose.server.yml` — 生产服务器：外部网络 `icebears-net`，不发布宿主端口
- `docker-compose.nas.yml` — **NAS：本文档使用**，不写死容器名，兼容老版 DSM

---

## 一、前置条件

| 项目 | 要求 |
| --- | --- |
| CPU 架构 | x86_64（Intel/AMD 型号）可直接 build；ARM 型号也能 build，实测 DS423 编译约 25s |
| 网络 | **NAS 需能访问 Docker Hub**，拉 `golang:1.23-alpine` / `alpine:3.20` 基础镜像 |
| 群晖 | DSM 7.x，启用「容器管理器（Container Manager）」；**需手动开启 SSH** |
| 端口 | 7865 未被占用 |

### 群晖上与常规 Linux 的三个关键差异

这三点是实测踩出来的，**别按常规 Linux 的经验来**：

1. **默认不带 `git`** —— 直接 `git clone` 会报 `git: command not found`。
   本文档的部署流程**不依赖 NAS 上的 git**；若确实要用，去套件中心装 `Git Server`。
2. **Docker 需要 `sudo`** —— 普通用户不在 docker 组，直接跑会报
   `permission denied ... /var/run/docker.sock`。两种解法：
   ```bash
   sudo docker compose -f docker-compose.nas.yml up -d --build   # 每次加 sudo
   # 或把自己加入 docker 组（改完需重新登录 SSH 才生效）：
   sudo synogroup --add docker "$USER"
   ```
3. **SSH 默认关闭且不自动启动** —— 去 **控制面板 → 终端机和 SNMP → 终端机**
   勾选「启动 SSH 功能」。若连不上，检查 **控制面板 → 安全性 → 防火墙**，
   以及「自动封锁」名单里有没有你的 IP。

### 先确认架构与端口

```bash
uname -m                          # x86_64 或 aarch64
sudo netstat -tulnp | grep 7865   # 无输出说明端口空闲
```

> ⚠️ **ARM 架构注意**：镜像需在 NAS 本机构建（本文档流程天然如此）。不要从 x86 机器
> `docker save` 后导入，amd64 镜像在 arm64 上无法运行。若 NAS 构建过慢，改用本机
> `docker buildx build --platform linux/arm64` 交叉构建后导入。

---

## 二、部署步骤

以下命令均在 NAS 的 SSH 会话中执行。

### 1. 放置源码

先确定项目要放哪个存储卷。群晖最多有多块盘/存储池，Docker 项目建议放在 SSD 卷上
（例如 `/volume2/docker_ssd/`），**下文一律用 `$PROJ` 代指该目录**，按实际情况替换：

```bash
PROJ=/volume2/docker_ssd/workbuddy2api   # ← 改成你自己的路径
sudo mkdir -p "$PROJ"
sudo chown -R "$(id -u)":"$(id -g)" "$PROJ"
cd "$PROJ"
```

**方式一：`git clone`（需先在套件中心装 `Git Server`）**

```bash
git clone <你的仓库地址> .
```

**方式二：本机打包传过去（推荐，不依赖 NAS 上的 git）**

在本机（Windows Git Bash）执行：

```bash
# 只打包源码，排除凭证与本地状态
tar czf /tmp/wb2api-src.tar.gz \
  --exclude='./.git' --exclude='./auths' --exclude='./data' --exclude='./config.json' .

# ⚠️ 关键：Windows 是 CRLF 行尾，必须转换，否则 NAS 上会连环报错（见「踩坑清单」第 7 条）
tar xzf /tmp/wb2api-src.tar.gz -C /tmp/wb2api-lf
find /tmp/wb2api-lf -type f \
  \( -name '*.sh' -o -name '*.json' -o -name '*.yml' -o -name '*.md' -o -name '*.go' \) \
  -exec sed -i 's/\r$//' {} +
tar czf /tmp/wb2api-src.tar.gz -C /tmp/wb2api-lf .

scp /tmp/wb2api-src.tar.gz <用户名>@<NAS-IP>:/volume2/docker_ssd/workbuddy2api/   # 换成你的路径
```

> 传文件也可走 SMB：资源管理器输入 `\\<NAS-IP>`，进共享文件夹直接拖。
> 注意 `\\<NAS-IP>\<共享名>` 映射到的**真实卷路径**未必和 SSH 里看到的一致，
> 拖完用 `ls -la "$PROJ"` 确认真到了预期位置。

再到 NAS 上解包：

```bash
cd "$PROJ"
tar xzf wb2api-src.tar.gz && rm wb2api-src.tar.gz
```

### 2. 建配置与数据目录

```bash
cp config.example.json config.json
mkdir -p auths data
```

> ⚠️ **`config.json` 必须存在**，见「踩坑清单」第 1 条。

### 3. 改 `config.json`

至少改掉网关密钥（`api_key`），这个值就是你以后给各客户端用的 `Authorization: Bearer <key>`。

生成一个随机密钥：

```bash
python3 -c "import secrets;print(secrets.token_hex(16))"
```

替换进配置（**推荐这种非交互方式，避免编辑器引入问题**）：

```bash
sed -i 's/"your-api-key-here"/"<上面生成的随机串>"/' config.json
sed -i 's/\r$//' config.json                     # 清掉可能存在的 CR
python3 -m json.tool config.json > /dev/null && echo "✅ JSON 合法"
```

其余字段保持默认即可：

```jsonc
{
  "listen": ":7865",
  "api_key": "<你的随机串>",       // ← 客户端就用这个
  "auth_dir": "./auths",
  "state_file": "./data/state.json"
}
```

> 若用 `vi` 手改，改完同样跑一遍 `sed -i 's/\r$//' config.json` 和
> `python3 -m json.tool` 校验。

### 4. 修权限（最容易反复踩的一步）

镜像内以 **uid 10001（app 用户）**运行，容器**只需读**源码与配置、**需要写** `data/`。
只要"读的能读、写的能写"，权限就对了。一条命令覆盖全部：

```bash
cd "$PROJ"
chmod 755 .                 # 父目录必须可进入，否则里面所有文件都读不到
chmod -R a+r .              # 全部文件对所有人可读（容器只读，放宽无风险）
sudo chown -R 10001:10001 data   # 只有 data 需要容器可写
```

完成后用 `ls -la` 检查，关键看这四项：

| 对象 | 期望 | 说明 |
| --- | --- | --- |
| `$PROJ`（项目目录） | `drwxr-xr-x` | **必须 `a+x`**，否则容器连目录都进不去 |
| `config.json` | 含 `r` for others | 如 `-rw-r--r--` |
| `auths/` 及其内文件 | 含 `r` | 如 `drwxr-xr-x` / `-rw-r--r--` |
| `data/` | 属主 `10001`，含 `w` | 如 `drwxrwxrwx` |

> ⚠️ **三个反直觉之处**（实测踩过）：
>
> 1. **父目录权限是隐形杀手**。文件本身 777 也可能读不到——若项目目录是
>    `drwx------`（700），容器什么都访问不了，日志报 `permission denied`。
> 2. **`chown` 成 10001 后，你自己反而不方便了**。你（`iSunker`）不是属主，
>    删改这些文件需 `sudo`，`chmod` 会报 `Operation not permitted` —— 这是正常的，
>    不是出错。
> 3. **权限会反复失效**。每次你在 NAS 上 `cp`/`vi`/解包/重新拖拽产生新文件，
>    属主和权限都会重置。**改了文件就重跑一遍上面三条命令**。

若 `chmod` 因群晖 ACL 不生效（`ls -la` 输出带 `+` 号），用群晖自己的工具查看/清除：

```bash
sudo synoacltool -get "$PROJ"
sudo synoacltool -del "$PROJ"      # 谨慎：清掉 ACL 后按传统权限位生效
```

实在懒得处理权限，可在 `docker-compose.nas.yml` 里打开 `user: "0:0"` 让容器以 root 运行
（能用，但放弃最小权限设计，不推荐）。

### 5. 添加凭证

**推荐：启动后用管理后台「自动授权登录」** —— 不用找 key，浏览器登录即可：

启动完成后打开 `http://<NAS-IP>:7865/admin` →「自动授权登录」→ 选中国版
（`copilot.tencent.com`）或国际版（`www.codebuddy.ai`）。

**或用 `ck_` Key**（key 从 CodeBuddy 客户端/后台获取，形如 `ck_xxxxxxxx`）：

```bash
./addkey.sh ck_你的真实key 备注名
```

生成 `auths/codebuddy-备注名.json`。不传备注名时按 key 校验和自动命名。

> ⚠️ **不要把示例里的中文当参数照抄**。`./addkey.sh ck_你的key 备注名` 会把
> 字面量 `ck_你的key` 写进凭证文件——那是个无效 key，服务会正常启动但拉模型时
> 报 `http 401: {"message":"invalid_format"}`。真 key 必须是 `ck_` 开头的实际字符串。

添加后在管理后台点「**重载目录**」，或重启容器生效。

### 6. 构建并启动

```bash
sudo docker compose -f docker-compose.nas.yml up -d --build
```

> 群晖上 Docker 需要 `sudo`（见「前置条件」第 2 点）。

群晖图形界面方式：**容器管理器 → 项目 → 新增** → 路径填 `$PROJ`
→ 由于目录下同时存在 `docker-compose.yml`，需在界面上手动把编排文件指定为
`docker-compose.nas.yml` → 下一步 → 完成。

**实测耗时参考**（DS423，ARM）：拉镜像 + 编译共约 **157s**，其中 `go build` 约 25s。
首次构建后镜像层有缓存，后续重建快得多。

---

## 三、验证

```bash
cd "$PROJ"

# 1) 容器状态：应为 Up (healthy)
sudo docker ps --filter name=workbuddy2api-cb2api-1

# 2) 凭证状态（权威指标，见下）
sudo docker exec workbuddy2api-cb2api-1 wget -qO- http://127.0.0.1:7865/healthz

# 3) 端到端实测：真发一次对话请求
KEY=$(python3 -c "import json;print(json.load(open('config.json'))['api_key'])")
curl -s -X POST http://<NAS-IP>:7865/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"说一个字：好"}],"max_tokens":10}'
```

**实测的正常输出**：

```json
// 第 2 步：accounts / healthy 都应为 1
{"accounts":1,"has_api_key":true,"healthy":1,"status":"ok","upstream_base":"https://copilot.tencent.com"}

// 第 3 步：拿到模型真实回复
{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"好","role":"assistant"}}],
 "model":"auto","usage":{"prompt_tokens":18,"completion_tokens":1,"total_tokens":19}}
```

**判断标准（三者层层递进）**：

| 指标 | 含义 |
| --- | --- |
| `healthz` 的 `accounts` | 加载到的凭证文件数。为 0 说明没凭证 |
| `healthz` 的 `healthy` | 其中可用的数量。为 0 说明凭证都不可用 |
| 第 3 步的回复 | **唯一可信的端到端验证** —— 能返回内容即真打通 |

> ⚠️ **不要用 `/v1/models` 判断凭证是否有效**。该接口有内置 fallback 模型表，
> **0 个凭证时照样返回完整模型列表**，看起来"正常"实则根本没账号可用。
> 详见「踩坑清单」第 9 条。

常见失败返回：

```json
{"error":{"code":"no_healthy_account","message":"all accounts unavailable (cooling/disabled)"}}
```
→ 凭证数为 0 或全部不可用，见「踩坑清单」第 8 条。

浏览器打开 `http://<NAS-IP>:7865/admin` 能看到管理页即部署成功。

**客户端接入**：Base URL 填 `http://<NAS-IP>:7865/v1`，鉴权头
`Authorization: Bearer <你的 api_key>`，模型名用 `/v1/models` 返回的 `id`。
兼容 Cherry Studio / LobeChat / NextChat / Open WebUI 等任何支持 OpenAI Chat 的客户端。

---

## 四、踩坑清单

### 1. 漏建 `config.json` → 挂载点变成目录

compose 里写的是 `./config.json:/app/config.json:ro`。若宿主上 `config.json` **不存在**，
Docker 会**自动创建一个同名目录**挂进去，程序读到目录而非文件，启动失败。

现象：`docker logs` 报读取配置失败；宿主上 `config.json` 是个目录（`ls -ld` 能看到 `d` 开头）。

处理：

```bash
sudo docker compose -f docker-compose.nas.yml down
rm -rf config.json && cp config.example.json config.json
sed -i 's/\r$//' config.json
# 改好 api_key 后：
sudo docker compose -f docker-compose.nas.yml up -d
```

### 2. 权限问题 → 容器读不到配置 / 写不进状态

**这是实测中最容易反复踩、也最不容易一眼看穿的坑。** 分两种表现：

```
load config: read config: open /app/config.json: permission denied   # 读不到
# 或写 state.json 时报 permission denied                          # 写不了
```

**常被忽略的是父目录**：即使 `config.json` 本身是 777，只要项目目录是
`drwx------`（700），容器（uid 10001）连目录都进不去，一样读不到文件。

一揽子修复（见「部署步骤 4」，改完文件后重跑）：

```bash
cd "$PROJ"
chmod 755 .                      # 父目录可进入 —— 关键
chmod -R a+r .                   # 全部可读
sudo chown -R 10001:10001 data   # data 需容器可写
```

**注意 `chmod` 报 `Operation not permitted` 不是错误** —— 那些文件属主已是 10001，
你不是属主，所以改不了；只要权限位本来就够用（如 755），无需处理，加 `sudo` 也行。

### 3. 容器名冲突 / 误用 server 编排

`docker-compose.server.yml` 声明了 `networks.icebears-net: external: true`，
NAS 上没有这个网络，直接用会报：

```
network icebears-net declared as external, but could not be found
```

**NAS 上只用 `docker-compose.nas.yml`。** 本文件刻意不写 `container_name`，
容器名由 compose 生成为 `<项目名>-cb2api-1`（项目名取目录名时即
`workbuddy2api-cb2api-1`），从而不会与服务器上的 `codebuddy2api` 或手工
`docker run` 创建的同名容器打架。

> 因此根目录 `deploy.sh` 中按 `codebuddy2api` 这个名字做的检查/备份逻辑，
> 在 NAS 上**不适用**。NAS 侧更新请见第六节。

### 4. 端口被占用

群晖自身服务占 5000/5001/7000 等，一般不占用 7865。若冲突，改
`docker-compose.nas.yml` 里 `ports` 的**左侧**（宿主端口）即可，容器内始终监听 7865：

```yaml
ports:
  - "17865:7865"    # 宿主用 17865，容器内不变
```

若改了宿主端口，客户端 Base URL 也要跟着改成 `http://<NAS-IP>:17865/v1`。

### 5. 改了 `config.json` 不生效

配置以只读方式挂载（`:ro`），且程序启动时读一次。改完必须重启容器：

```bash
sudo docker compose -f docker-compose.nas.yml restart
```

### 6. 编排文件版本字段的兼容性

`docker-compose.nas.yml` 顶部保留了 `version: "3.8"`。**新版 compose（v2 起）会警告**
`the attribute version is obsolete, it will be ignored` —— **可以无视**：
该字段在新版被忽略，在老版解析器上则是必需的顶层声明，
保留它等于同时兼容新旧两侧，代价只是一个警告。

`mem_limit` 字段在部分群晖 compose 版本上不被支持、会直接报错，因此文件里
默认注释掉了；确认你的 DSM 能吃再打开。

### 7. Windows 换行符（CRLF）→ 一串迷惑报错

**从 Windows 传到 NAS 的文件是 CRLF 行尾，Linux 下会引发三类看似无关的错误**，
实测全部踩过：

| 现象 | 根因 |
| --- | --- |
| `/usr/bin/env: 'bash\r': No such file or directory` | shell 脚本 shebang 行末多了 `\r`，找 `bash\r` 这个程序 |
| `load config: parse config: invalid character 'ï' after object key:value pair` | JSON 行尾的 `\r` 是非法字符，Go 报的位置还带有误导性 |
| 脚本行为诡异 / 参数带 `\r` | 同类问题 |

**注意**：`ï` **不是 BOM**。用 `head -c 3 config.json | xxd` 可排除 BOM
（BOM 会显示 `ef bb bf`；正常应是 `7b` 即 `{`）。

一次性修复（在 NAS 上对项目目录执行）：

```bash
cd "$PROJ"
find . -type f \( -name '*.sh' -o -name '*.json' -o -name '*.yml' -o -name '*.md' -o -name '*.go' \) \
  -exec sed -i 's/\r$//' {} +
python3 -m json.tool config.json > /dev/null && echo "✅ config.json 合法"
```

**从源头避免**：本机打包时先转换行尾再打包（见「部署步骤 1 · 方式二」），
比事后在 NAS 上救火省事得多。

### 8. 假凭证 → 服务正常但拉模型报 401

如果把文档里的中文占位符照抄进命令：

```bash
./addkey.sh ck_你的key 备注名        # ❌ 字面量，无效
```

会生成一个 `api_key` 值为 `ck_你的key` 的凭证文件。**服务能正常启动、健康检查也通过**，
但实际发对话请求时报：

```
models: all accounts fetch failed (models /v3/config http 401: {"message":"invalid_format"})
```

处理（**先看内容确认是假凭证，再删**）：

```bash
ls auths/                                   # 有哪些凭证文件
sudo cat auths/codebuddy-备注名.json        # 确认 api_key 是不是中文占位符
sudo rm auths/codebuddy-备注名.json         # 确认后再删
./addkey.sh ck_真实key 我的账号              # 真 key 是 ck_ 开头的实际字符串
```

或直接用管理后台「自动授权登录」（见「部署步骤 5」）。

### 9. 别用 `/v1/models` 判断凭证是否有效

**`GET /v1/models` 有内置 fallback 模型表**：即使 `auths/` 里一个凭证都没有，
它照样返回完整模型列表（实测 19 个）。因此：

```
模型列表能返回  ≠  凭证有效    ← 这是个陷阱
```

**凭证状态的权威指标**：

```bash
# accounts = 加载到的凭证数；healthy = 其中可用的数量
sudo docker exec workbuddy2api-cb2api-1 wget -qO- http://127.0.0.1:7865/healthz
# {"accounts":1,"has_api_key":true,"healthy":1,...}   ← 正常

sudo docker logs --tail 20 workbuddy2api-cb2api-1 | grep credential
# loaded 1 credential(s) from /app/auths               ← 正常
# loaded 0 credential(s) ... no credentials yet        ← 没凭证
```

`accounts:0` 时发对话请求会得到：

```json
{"error":{"code":"no_healthy_account","message":"all accounts unavailable (cooling/disabled)"}}
```

**唯一可信的端到端验证**是真发一次 `POST /v1/chat/completions`（见「三、验证」第 3 步）。

### 10. 凭证文件：删不得，先备份

`auths/` 下的凭证文件**属主是容器用户 uid 10001**，你的普通用户读不了，
所以：

- 查看内容要 `sudo cat`，删除要 `sudo rm`
- **普通 `tar` 备份会因权限被跳过** —— 必须加 `sudo`：

```bash
cd "$PROJ"
# ❌ 会漏掉 auths/（tar 报 Cannot open: Permission denied）
tar czf ~/backup.tar.gz auths data config.json

# ✅ 正确：加 sudo，并验证包内确实含凭证
sudo tar czf /var/services/homes/$USER/cb2api-backup-$(date +%F).tar.gz auths data config.json
sudo chown "$USER:$USER" /var/services/homes/$USER/cb2api-backup-$(date +%F).tar.gz
tar tzf /var/services/homes/$USER/cb2api-backup-$(date +%F).tar.gz | grep auths/
```

> ⚠️ 最后一步**必须能看到 `auths/codebuddy-*.json`**，否则备份是残缺的、
> 关键时刻恢复不了。

**删除凭证是高危操作**：文件里可能承载着 OAuth 绑号结果（如 Apple ID 登录产生的
`codebuddy-xxx@privaterelay.appleid.com.json`）。删掉就得重新绑号。
**删之前务必先 `ls` + `sudo cat` 确认内容，或先做一份带 `sudo` 的备份。**

---

## 五、安全提醒

- **`/admin` 会向页面注入真实网关 Key**（`window.__API_KEY__`），务必只在内网开放，
  不要直接端口映射到公网。需要外网访问时，套 NAS 自带的反向代理并自加一层鉴权。
- `auths/`、`data/`、`config.json` 三者均含敏感信息且**不在 git 里**，迁移或备份时
  注意加密，不要随手丢进公共网盘。
- **不要用同一个 `ck_` Key 在两台机器上并发跑**（例如本机 Docker 和 NAS 同时用一份凭证），
  容易触发上游限流风控，两台都受影响。NAS 上建议单独 `addkey.sh` 添加独立 Key。

---

## 六、日常更新与备份

NAS 侧**不用 `deploy.sh`**（它按固定容器名工作）。推荐的更新闭环是
「**本机改代码 → 本机验证 → 打包传 NAS → 解包重启**」：

**本机**（Git Bash）：

```bash
# 1. 改完代码，本地跑测试
go test ./...

# 2. 打包（含 CRLF→LF 转换，见「部署步骤 1 · 方式二」）
tar czf /tmp/wb2api-src.tar.gz \
  --exclude='./.git' --exclude='./auths' --exclude='./data' --exclude='./config.json' .
tar xzf /tmp/wb2api-src.tar.gz -C /tmp/wb2api-lf
find /tmp/wb2api-lf -type f \( -name '*.sh' -o -name '*.json' -o -name '*.yml' -o -name '*.go' \) \
  -exec sed -i 's/\r$//' {} +
tar czf /tmp/wb2api-src.tar.gz -C /tmp/wb2api-lf .
scp /tmp/wb2api-src.tar.gz <用户名>@<NAS-IP>:/volume2/docker_ssd/workbuddy2api/
```

**NAS 上**：

```bash
cd "$PROJ"
tar xzf wb2api-src.tar.gz && rm wb2api-src.tar.gz   # 覆盖代码（不动 config/auths/data）

chmod 755 . && chmod -R a+r .                        # 新文件权限会重置，重跑一遍
python3 -m json.tool config.json > /dev/null && echo "✅ JSON 仍合法"

sudo docker compose -f docker-compose.nas.yml up -d --build
sudo docker logs --tail 15 workbuddy2api-cb2api-1
```

> ⚠️ `tar` 解包**只覆盖同名文件，不会删除**已从源码中移除的文件。
> 若本次改动删掉了某些文件，NAS 上会残留旧副本；对 Go 编译一般无影响，
> 需要彻底干净时用 `sudo docker compose ... down` 后从头解包。

凭证/状态/配置是挂载进容器的宿主文件，**重建容器不会丢账号和冷却状态**。

### 回滚

> ⚠️ 注意：根目录 `deploy.sh` 那套「自动备份镜像 → `--rollback`」的回滚机制
> **在 NAS 上不适用**。`deploy.sh` 会在重建前把当前镜像打成
> `codebuddy2api:rollback-<时间戳>` tag，而 `docker-compose.nas.yml` 只是固定
> 使用 `workbuddy2api-nas:latest`，**没有任何打 tag 的动作** —— 每次
> `up -d --build` 都会直接覆盖 `latest`，旧镜像随即变成悬空镜像（`<none>`），
> 因此并不存在「上一个 tag」可切。

两条真正可用的回滚路径：

**方式一：源码回退后重建（最可靠）**

本机保留最近一两版 tar 包，回滚时重新传过去解包重建：

```bash
cd "$PROJ"
tar xzf wb2api-src.tar.gz && rm wb2api-src.tar.gz   # 换成上一版的包
chmod 755 . && chmod -R a+r .
sudo docker compose -f docker-compose.nas.yml up -d --build
```

`auths/`、`data/`、`config.json` 是挂载文件，回退源码不会动它们。

**方式二：更新前主动留镜像 tag（想随时切回旧镜像时用）**

更新前先把当前镜像存一份：

```bash
sudo docker tag workbuddy2api-nas:latest workbuddy2api-nas:rollback-$(date +%F-%H%M)
```

需要回滚时，把镜像名对调即可：

```bash
sudo docker tag workbuddy2api-nas:rollback-<你记下的时间戳> workbuddy2api-nas:latest
sudo docker compose -f docker-compose.nas.yml up -d --no-build
```

方式二不用动源码，但**必须在更新前主动执行** —— 忘了打 tag 就只能走方式一。

### 备份

只需这三个（`auths/`、`data/`、`config.json` 就是镜像外的全部状态）。
**必须加 `sudo`** —— 凭证文件属主是 uid 10001，普通用户读不到（详见「踩坑清单」第 10 条）：

```bash
cd "$PROJ"
sudo tar czf /var/services/homes/$USER/cb2api-backup-$(date +%F).tar.gz auths data config.json
sudo chown "$USER:$USER" /var/services/homes/$USER/cb2api-backup-$(date +%F).tar.gz

# 验证包内确实含凭证，否则备份无效
tar tzf /var/services/homes/$USER/cb2api-backup-$(date +%F).tar.gz | grep auths/
```

---

## 七、排错命令速查

以下命令均在 `$PROJ` 目录下执行，注意群晖需要 `sudo`。
容器名固定为 `workbuddy2api-cb2api-1`（compose 按「项目名-服务名-序号」生成），
也可以直接用 `$(sudo docker compose -f docker-compose.nas.yml ps -q cb2api)` 取 ID。

```bash
sudo docker ps --filter name=workbuddy2api-cb2api-1     # 状态（看 Up/healthy）
sudo docker logs --tail 50 workbuddy2api-cb2api-1       # 最近日志
sudo docker logs -f workbuddy2api-cb2api-1              # 实时日志（Ctrl+C 退出）
sudo docker exec -it workbuddy2api-cb2api-1 sh          # 进容器

sudo docker compose -f docker-compose.nas.yml restart   # 重启
sudo docker compose -f docker-compose.nas.yml down      # 停止并删除容器（不动挂载目录）
sudo docker compose -f docker-compose.nas.yml config    # 校验编排文件语法
```

进容器后可直接验证内置资源：

```bash
wget -qO- http://127.0.0.1:7865/healthz
```

**按日志关键字定位问题**：

| 日志内容 | 去看 |
| --- | --- |
| `permission denied` | 踩坑清单 第 2 条（权限，注意父目录） |
| `invalid character 'ï'` | 踩坑清单 第 7 条（CRLF 行尾） |
| `http 401 / invalid_format` | 踩坑清单 第 8 条（凭证无效） |
| `no such file or directory`（compose 报） | 确认在 `$PROJ` 下执行、文件名拼写正确 |
| `permission denied ... docker.sock` | 前置条件 第 2 点（加 `sudo`） |
