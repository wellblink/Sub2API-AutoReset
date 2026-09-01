# Sub2API-AutoReset

Sub2API-AutoReset 是一个独立运行在 [Sub2API](https://github.com/Wei-Shaw/sub2api) 旁边的 Docker sidecar，用于监听 OpenAI OAuth 上游账号的 7 天用量变化。当系统确认上游账号发生了**官方后台人工额度重置**时，它可以自动调用 Sub2API 管理接口，重置预先映射的下游订阅额度。

项目不会修改 Sub2API 官方镜像。Sub2API 正常拉取和更新后，本项目的配置与运行状态仍保留在独立目录中。

> [!WARNING]
> 自动监听开启后会执行真实的下游额度重置。首次部署默认关闭，请先完成账号映射并使用“立即检查”验证数据，再开启自动监听。

## 功能

- 只监听 Sub2API 中有效的 OpenAI OAuth 账号。
- 可为每个上游账号选择需要联动的下游订阅。
- 可分别控制日、周、月额度是否重置。
- 优先复用 Sub2API 已持久化且仍新鲜的上游用量，避免重复查询。
- 必须经过独立二次采样确认，且会排除自然周期重置、重置卡减少或更换等情况。
- 查询任务带后台随机时间偏移，不会让所有 OAuth 账号在同一时刻查询。
- 支持 Bark 和企业微信群机器人通知，支持自定义通知模板。
- 每次重置前后记录下游用户、订阅和用量快照。
- 支持在 24 小时内对单个下游用户执行安全回滚。
- 管理页面可通过 Sub2API 自定义页面嵌入，并使用当前管理员会话鉴权。

## 工作原理

普通轮询先读取 Sub2API 账号记录中的上游用量采样时间。如果样本仍在配置周期内，就直接复用 Sub2API 数据；样本过期或缺失时，才调用账号管理页面“查询”按钮所对应的强制刷新接口。

检测器比较同一个 7 天窗口内的真实累计值，包括：

- 请求数 `requests`
- Token 数 `tokens`
- 上游金额 `cost`
- 标准金额 `standard_cost`
- 用户金额 `user_cost`

只要其中任一累计值下降，就会成为候选事件。候选事件还必须满足：

1. 没有进入自然重置时间保护区间；
2. 重置卡数量没有减少；
3. 原有重置卡到期明细没有消失或被同数量新卡替换；
4. 延迟后进行的第二次独立强制查询仍确认用量下降；
5. 当前上游账号、分组及下游映射与保存时一致；
6. 同一上游重置窗口此前没有产生过联动事件。

因此，下列情况不会触发下游重置：自然周期到期、通过重置卡重置、重置卡缓存不完整、单次异常读数、账号分组变化、映射被关闭以及重复事件。

### 轮询能力边界

如果官方重置后，两个采样之间新增的用量已经超过重置前累计值，那么最终样本不会呈现下降，任何只依赖轮询的方法都无法识别该事件。缩短轮询周期可以降低该概率，但会增加上游查询频率。

## 环境要求

- 已运行的 Sub2API，且使用 PostgreSQL 和 Redis。
- Docker Engine 24+ 与 Docker Compose v2。
- sidecar 必须加入 Sub2API、PostgreSQL 和 Redis 所在的同一个 Docker 网络。
- 一个有效的 Sub2API 管理员 API Key。
- 如果需要嵌入 Sub2API 管理页面，需要可修改站点反向代理配置。

## 快速安装

### 1. 准备目录

建议将项目克隆到现有 Sub2API Compose 文件旁边：

```bash
git clone https://github.com/wellblink/Sub2API-AutoReset.git quota-sync
cd quota-sync
mkdir -p data secrets
chmod 700 data secrets
```

不要把真实密钥写入 `.env` 或 Compose 文件。将管理员 API Key 写入 Docker secret 文件：

```bash
printf '%s' '请替换为你的 Sub2API 管理员 API Key' > secrets/sub2api_admin_api_key
chmod 600 secrets/sub2api_admin_api_key
```

### 2. 合并 Compose 服务

打开 [deploy/compose-service.example.yml](deploy/compose-service.example.yml)，将其中的 `quota-sync` 服务合并到你现有 Sub2API 的同一个 `docker-compose.yml` 中，并在文件顶层合并 `secrets` 定义。

示例默认假设现有服务名和网络名如下：

| 组件 | 默认名称 |
| --- | --- |
| Sub2API 服务 | `sub2api` |
| PostgreSQL 服务 | `postgres` |
| Redis 服务 | `redis` |
| Docker 网络 | `sub2api-network` |

如果你的名称不同，请同步修改示例中的主机名、`depends_on` 和 `networks`。

如果从 Docker Hub 使用预构建镜像，保留：

```yaml
image: wellblink/sub2api-autoreset:latest
```

如果希望本地构建，删除 `image`，改用：

```yaml
build:
  context: ./quota-sync
  dockerfile: Dockerfile
```

### 3. 启动

在现有 Sub2API Compose 目录执行：

```bash
docker compose pull quota-sync
docker compose up -d --no-deps quota-sync
docker compose ps quota-sync
docker compose logs --tail=100 quota-sync
```

健康检查应显示 `healthy`。也可以从宿主机验证：

```bash
curl http://127.0.0.1:8091/healthz
```

## 环境变量

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `TZ` | `Asia/Shanghai` | 日额度边界和日志时区，应与 Sub2API 一致 |
| `SUB2API_BASE_URL` | `http://sub2api:8080/api/v1` | Sub2API 内部管理 API 地址 |
| `SUB2API_ADMIN_API_KEY_FILE` | `/run/secrets/sub2api_admin_api_key` | 管理员 API Key 文件路径 |
| `QUOTA_SYNC_LISTEN` | `:8090` | sidecar 容器监听地址 |
| `QUOTA_SYNC_STATE_PATH` | `/data/state.json` | 配置、采样和事件记录文件 |
| `DATABASE_HOST` | `postgres` | Sub2API PostgreSQL 主机 |
| `DATABASE_PORT` | `5432` | PostgreSQL 端口 |
| `DATABASE_USER` | `sub2api` | PostgreSQL 用户 |
| `DATABASE_PASSWORD` | 空 | PostgreSQL 密码，通常复用现有 Compose 变量 |
| `DATABASE_DBNAME` | `sub2api` | PostgreSQL 数据库 |
| `DATABASE_SSLMODE` | `disable` | 数据库 TLS 模式 |
| `REDIS_HOST` | `redis` | Sub2API Redis 主机 |
| `REDIS_PORT` | `6379` | Redis 端口 |
| `REDIS_USERNAME` | 空 | Redis 用户名 |
| `REDIS_PASSWORD` | 空 | Redis 密码 |
| `REDIS_DB` | `0` | Redis 数据库编号 |
| `REDIS_ENABLE_TLS` | `false` | 是否启用 Redis TLS |

数据库写权限只用于撤销事务；正常联动重置通过 Sub2API 官方管理员接口完成。Redis 用于清除计费缓存并发布订阅缓存失效消息。

## 嵌入 Sub2API 管理页面

管理页依赖 Sub2API 浏览器管理员会话，不应直接暴露 sidecar 的 8090 端口到公网。推荐只绑定宿主机回环地址，并通过 Sub2API 同域名反向代理。

### 1. 添加同源反向代理

将 [deploy/nginx-location.example.conf](deploy/nginx-location.example.conf) 中的内容合并到 Sub2API 对应的 Nginx `server` 块，随后执行：

```bash
nginx -t && systemctl reload nginx
```

如果 Nginx 本身也运行在容器中，请将 `proxy_pass` 改为它能够访问的 sidecar 地址。

### 2. 创建自定义页面

在 Sub2API 管理后台创建自定义页面，页面标识使用 `quota-auto-reset`，标题使用“自动重置”，Markdown 内容复制自 [deploy/quota-auto-reset.md](deploy/quota-auto-reset.md)。

自定义菜单链接应指向：

```text
/custom/quota-auto-reset
```

菜单定位脚本由 sidecar 的 `/menu.js` 提供。Nginx 示例会把它注入 Sub2API HTML，使“自动重置”菜单移动到“账号管理”下面。若未来 Sub2API 调整侧栏 DOM，菜单仍会保留在官方自定义菜单区域，只是可能无法自动移动。

### 3. 首次设置

1. 以管理员身份登录 Sub2API。
2. 打开“自动重置”。
3. 在“账号映射”选择需要监听的 OAuth 上游账号。
4. 为每个上游选择允许联动的下游订阅及日/周/月额度。
5. 保存映射。
6. 在“概览与设置”保存检测参数。
7. 点击“立即检查”确认账号与用量读取正常。
8. 最后开启自动监听并保存。

每个标签的保存按钮只保存当前标签；未保存就切换标签，页面会恢复已持久化的值。

## 检测参数

- **轮询周期（秒）**：后台调度的基础周期，实际查询会加入随机正偏移。
- **二次确认延迟（秒）**：发现下降候选后，等待多久再进行强制确认。
- **自然重置保护窗口（秒）**：靠近上游自然重置时间时不联动。
- **样本最大年龄（秒）**：超过该年龄的 Sub2API 持久化样本必须主动刷新。

不要把轮询周期设置得过短。建议先使用默认值观察运行记录，再根据上游账号数量和业务使用频率调整。

## 通知

支持两个通知渠道：

- Bark V2 JSON `POST /push`，支持官方或自建 Bark Server。
- 企业微信群机器人文本 Webhook。

每个渠道均可接收三类消息：检测到上游重置、完成下游重置、完成单用户回滚。通知标题和正文支持页面中列出的模板变量。密钥只存放在 `data/state.json`，管理 API 返回时只显示脱敏后的开头和结尾。

请限制 `data` 目录权限并定期备份，不要将它加入 Git 或发送给他人。

## 撤销规则

每个成功重置的下游订阅可以在 24 小时内独立撤销一次。撤销会保留重置后新增的消费，并按日、周、月三个窗口分别判断：

- 同一自然日内，日额度恢复为“重置前用量 + 重置后新增用量”。
- 跨过自然日后，昨天的日用量不会写入今天；今日用量保持当前值。
- 原周/月窗口仍有效时，周/月额度恢复为“重置前用量 + 重置后新增用量”。
- 某个周期已经自然到期时，只跳过该周期，不影响其他仍有效周期。
- 原周期仍有效但窗口锚点被其他重置改变时，系统拒绝覆盖，避免破坏后续数据。

撤销会直接访问 Sub2API PostgreSQL 和 Redis，因此务必确保 `TZ`、数据库和 Redis 配置与 Sub2API 完全一致。

## 更新

更新本项目不会影响 Sub2API：

```bash
docker compose pull quota-sync
docker compose up -d --no-deps quota-sync
```

更新 Sub2API 也不会覆盖本项目：

```bash
docker compose pull sub2api
docker compose up -d --no-deps sub2api
```

建议更新前备份：

```bash
cp -a quota-sync/data "quota-sync/data.backup.$(date +%Y%m%d-%H%M%S)"
```

## 安全建议

- 不要将 sidecar 端口直接发布到公网。
- 使用 Docker secret 挂载管理员 API Key。
- `data/state.json` 包含通知凭据、账号映射和运行记录，应设为仅管理员可读。
- 不要提交 `.env`、`data/`、`secrets/`、数据库备份或日志。
- 定期轮换 Sub2API 管理员 API Key、Bark Key 和企业微信 Webhook。
- 首次启用前先核对账号分组与下游订阅映射。

## 故障排查

### 页面显示 401

确认当前浏览器已经登录 Sub2API 管理员账号，并检查 `/quota-sync/` 是否与 Sub2API 使用同一域名、协议和端口。

### 容器无法连接 Sub2API、PostgreSQL 或 Redis

确认四个服务在同一个 Docker 网络，并且环境变量中的主机名等于 Compose 服务名：

```bash
docker compose exec quota-sync wget -qO- http://sub2api:8080/health
docker compose logs --tail=200 quota-sync
```

### 没有可映射账号

只有平台为 OpenAI、类型为 OAuth、状态有效并且能关联到有效订阅分组的账号才会显示。

### 一直只建立采样但不联动

首次自动采样会建立比较基线。请同时确认 Sub2API 中已经点击过“次数”，使重置卡数量与到期明细完成持久化；明细不完整时检测器会安全地拒绝联动。

### 上游已重置但没有检测到

如果两个轮询样本之间的新消费已经超过重置前累计值，最终数据不会下降，检测器无法确认官方重置。可以适当缩短轮询周期。

## 本地开发

```bash
go test ./...
docker build -t sub2api-autoreset:dev .
```

项目使用 Go 1.24，前端页面以嵌入资源方式编译进单个静态二进制。

## 免责声明

本项目是社区 sidecar，不是 Sub2API 或 OpenAI 官方组件。上游接口和数据库结构变化可能导致兼容性问题。请先在测试环境验证，并自行承担自动重置和数据回滚带来的业务风险。

## 许可证

[MIT License](LICENSE)
