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
| CPU 架构 | x86_64（Intel/AMD 型号）可直接 build；ARM 型号（DS223 等）也能 build，只是慢 |
| 网络 | 能访问 GitHub（拉源码）+ Docker Hub（拉 `golang:1.23-alpine` / `alpine:3.20` 基础镜像） |
| 群晖 | DSM 7.x，启用「容器管理器（Container Manager）」；建议同时开启 SSH |
| 端口 | 7865 未被占用 |

先在 NAS 上确认架构与端口：

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

```bash
sudo mkdir -p /volume1/docker/workbuddy2api
sudo chown -R "$(id -u)":"$(id -g)" /volume1/docker/workbuddy2api
cd /volume1/docker/workbuddy2api
git clone https://github.com/icebears111/workbuddy2api.git .
```

也可以先用 File Station 把整个目录上传，再在 NAS 上 `git remote set-url` 配好，效果一样。

### 2. 建配置与数据目录

```bash
cp config.example.json config.json
mkdir -p auths data
```

> ⚠️ **`config.json` 必须存在**，见「踩坑清单」第 1 条。

### 3. 改 `config.json`

至少改掉网关密钥（`api_key`），这个值就是你以后给各客户端用的 `Authorization: Bearer <key>`：

```bash
vi config.json
```

```jsonc
{
  "listen": ":7865",
  "api_key": "换成你自己的随机字符串",   // ← 改这里
  "auth_dir": "./auths",
  "state_file": "./data/state.json"
  // ... 其余保持默认即可
}
```

生成一个随机密钥（NAS 上跑，或本机 `openssl rand -hex 16` 生成后粘贴）：

```bash
python3 -c "import secrets;print(secrets.token_hex(16))"
```

### 4. 修目录权限

镜像内以 **uid 10001（app 用户）**运行，而群晖挂载目录属主通常是 `admin`，
不处理会导致写 `data/state.json` 报权限错误：

```bash
sudo chown -R 10001:10001 data
```

`auths/` 容器内只需读取，一般不必改属主；若仍报错可一并 `chown`。

### 5. 添加凭证

```bash
chmod +x addkey.sh
./addkey.sh ck_你的key 备注名
```

生成 `auths/codebuddy-备注名.json`。不传备注名时会按 key 的校验和自动命名。

也可以跳过脚本，启动后打开 `http://<NAS-IP>:7865/admin` →「自动授权登录」，
浏览器登录 CodeBuddy 完成绑号（中国版 `copilot.tencent.com` / 国际版 `www.codebuddy.ai`）。

### 6. 构建并启动

命令行方式：

```bash
docker compose -f docker-compose.nas.yml up -d --build
```

群晖图形界面方式：**容器管理器 → 项目 → 新增** → 路径填 `/volume1/docker/workbuddy2api`
→ 由于目录下同时存在 `docker-compose.yml`，需在界面上手动把编排文件指定为
`docker-compose.nas.yml` → 下一步 → 完成。

首次构建需下载 Go 工具链并编译，NAS 上通常 **3–10 分钟**，属正常。

---

## 三、验证

```bash
# 1) 容器状态：应为 Up (healthy)
docker compose -f docker-compose.nas.yml ps

# 2) 应用自检（容器内，不经宿主端口）
KEY=$(python3 -c "import json;print(json.load(open('config.json'))['api_key'])")
docker exec $(docker compose -f docker-compose.nas.yml ps -q cb2api) \
  wget -qO- --header="Authorization: Bearer $KEY" http://127.0.0.1:7865/v1/models

# 3) 从局域网另一台机器访问
curl http://<NAS-IP>:7865/healthz
```

浏览器打开 `http://<NAS-IP>:7865/admin` 能看到管理页即部署成功。

**客户端接入**：Base URL 填 `http://<NAS-IP>:7865/v1`，鉴权头
`Authorization: Bearer <你的 api_key>`，模型名用 `/v1/models` 返回的 `id`。
兼容 Cherry Studio / LobeChat / NextChat / Open WebUI 等任何支持 OpenAI Chat 的客户端。

---

## 四、踩坑清单

### 1. 漏建 `config.json` → 服务起不来

`config.json` 在 `.gitignore` 里，`git clone` 后**只有 `config.example.json`**。
compose 里写的是 `./config.json:/app/config.json:ro`，文件不存在时 Docker 会
**自动创建一个同名目录**挂进去，程序读到目录而非文件，直接启动失败。

现象：`docker logs` 里报读取配置失败；宿主上 `config.json` 变成一个目录。

处理：

```bash
docker compose -f docker-compose.nas.yml down
rm -rf config.json && cp config.example.json config.json && vi config.json
docker compose -f docker-compose.nas.yml up -d
```

### 2. `data/` 权限 → 状态写不进去

`Operation not permitted` / `permission denied` 出现在写 `state.json` 时，
就是 uid 10001 对宿主目录没有写权限。`sudo chown -R 10001:10001 data` 即可。

不便 `chown` 时，可在 `docker-compose.nas.yml` 里打开 `user: "0:0"` 以 root 运行
（能用，但放弃了镜像的最小权限设计，不推荐）。

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
docker compose -f docker-compose.nas.yml restart
```

### 6. 编排文件版本字段的兼容性

`docker-compose.nas.yml` 顶部保留了 `version: "3.8"`。**新版 compose（v2 起）会警告**
`the attribute version is obsolete, it will be ignored` —— **可以无视**：
该字段在新版被忽略，在老版解析器上则是必需的顶层声明，
保留它等于同时兼容新旧两侧，代价只是一个警告。

`mem_limit` 字段在部分群晖 compose 版本上不被支持、会直接报错，因此文件里
默认注释掉了；确认你的 DSM 能吃再打开。

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

NAS 侧没有用 `deploy.sh`（它按固定容器名工作）。手动更新即可：

```bash
cd /volume1/docker/workbuddy2api
git pull --ff-only
docker compose -f docker-compose.nas.yml up -d --build
```

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

```bash
cd /volume1/docker/workbuddy2api
git log --oneline -5                  # 找到上一个可用的 commit
git checkout <上一个commit>
docker compose -f docker-compose.nas.yml up -d --build
# 问题修复后回到分支：
git checkout main && git pull --ff-only
docker compose -f docker-compose.nas.yml up -d --build
```

`auths/`、`data/`、`config.json` 是挂载文件，回退源码不会动它们。

**方式二：部署前主动留 tag（想随时切回旧镜像时用）**

更新前先把当前镜像存一份：

```bash
docker tag workbuddy2api-nas:latest workbuddy2api-nas:rollback-$(date +%F-%H%M)
git pull --ff-only
docker compose -f docker-compose.nas.yml up -d --build
```

需要回滚时，把两条命令里的镜像名对调即可：

```bash
docker tag workbuddy2api-nas:rollback-<你记下的时间戳> workbuddy2api-nas:latest
docker compose -f docker-compose.nas.yml up -d --no-build
```

这种方式不用动 git 工作区，但需要**在更新前主动执行**——忘了打 tag 就只剩方式一。

### 备份

只需这三个（`auths/`、`data/`、`config.json` 就是镜像外的全部状态）：

```bash
tar czf cb2api-backup-$(date +%F).tar.gz auths data config.json
```

---

## 七、排错命令速查

```bash
docker compose -f docker-compose.nas.yml ps            # 状态
docker compose -f docker-compose.nas.yml logs -f cb2api # 实时日志
docker compose -f docker-compose.nas.yml restart        # 重启
docker compose -f docker-compose.nas.yml down           # 停止并删除容器（不动挂载目录）
docker compose -f docker-compose.nas.yml config         # 校验编排文件语法
docker exec -it $(docker compose -f docker-compose.nas.yml ps -q cb2api) sh  # 进容器
```

进容器后可直接验证内置资源：

```bash
wget -qO- http://127.0.0.1:7865/healthz
```
