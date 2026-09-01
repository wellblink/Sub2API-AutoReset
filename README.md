# Sub2API-AutoReset

Sub2API-AutoReset 是一个配合 [Sub2API](https://github.com/Wei-Shaw/sub2api) 使用的自动重置服务。

它会监听 OpenAI OAuth 上游账号的 7 天用量。当系统确认上游额度被官方工作人员从后台重置后，可以自动重置指定下游用户的订阅额度。

本项目单独运行，不修改 Sub2API 镜像。以后正常更新 Sub2API，不会覆盖自动重置服务的配置和运行记录。

> [!WARNING]
> 开启自动监听后，系统会执行真实的下游额度重置。首次安装时开关默认关闭。请先完成账号映射，再点击“立即检查”确认数据正常，最后再开启自动监听。

## 主要功能

- 监听 Sub2API 中有效的 OpenAI OAuth 账号。
- 每个上游账号可以单独选择要联动的下游订阅。
- 可以分别重置日额度、周额度和月额度。
- 优先使用 Sub2API 已经查到的用量，避免短时间内重复查询上游。
- 检测到用量下降后会再次查询确认，避免一次异常数据造成误重置。
- 自动排除自然周期重置、使用重置卡、重置卡明细异常等情况。
- 后台会为不同账号加入随机查询偏移，避免所有账号同时查询。
- 支持 Bark 和企业微信群机器人通知。
- 通知标题和内容可以自定义。
- 重置前后都会保存下游用户和用量记录。
- 24 小时内可以按单个下游用户回滚额度重置。
- 管理页面可以直接嵌入 Sub2API 后台。

## 选择安装方式

建议按下面的顺序选择：

| 顺序 | 安装方式 | 适合情况 |
| --- | --- | --- |
| 1，推荐 | 加入现有 `docker-compose.yml` | 大多数已经使用 Docker Compose 部署 Sub2API 的用户 |
| 2 | 手动拉取 Docker Hub 镜像 | 想先下载镜像，或者需要手动控制镜像版本 |
| 3 | 从 GitHub 下载源码并构建 | 需要修改源码、调试或自行构建镜像 |

无论选择哪一种方式，自动重置服务都需要连接 Sub2API、PostgreSQL 和 Redis。最稳妥的运行方式始终是把它放进 Sub2API 现有的 Compose 文件中。

## 安装前需要确认

开始前请确认：

1. Sub2API 已经正常运行。
2. Sub2API 使用 PostgreSQL 和 Redis。
3. 服务器已经安装 Docker 和 Docker Compose v2。
4. 你可以修改 Sub2API 当前使用的 `docker-compose.yml`。
5. 你知道当前 Compose 中 Sub2API、PostgreSQL、Redis 和网络的名称。

下面的示例使用这些名称：

| 项目 | 示例名称 |
| --- | --- |
| Sub2API 服务 | `sub2api` |
| PostgreSQL 服务 | `postgres` |
| Redis 服务 | `redis` |
| Docker 网络 | `sub2api-network` |

如果你的 Compose 使用了其他名称，需要在示例中一起修改。

## 方式一：加入现有 Docker Compose（推荐）

这种方式直接使用 Docker Hub 上已经构建好的镜像，不需要下载源码。执行 `docker compose up` 时，Compose 会自动拉取镜像。

以下操作都在 Sub2API 当前的 Compose 目录中完成，也就是存放 `docker-compose.yml` 的目录。

### 第一步：生成管理员 API Key

登录 Sub2API 管理后台，进入“系统设置”，找到“管理员 API Key”，生成一个新的 Key。

这个 Key 用于让自动重置服务调用 Sub2API 管理接口。请单独保存，后面只会写进 Docker secret 文件，不要直接写进 Compose。

### 第二步：创建数据和密钥目录

```bash
mkdir -p quota-sync/data quota-sync/secrets
chmod 700 quota-sync/data quota-sync/secrets
```

`quota-sync/data` 用于保存自动重置设置、账号映射、通知设置和运行记录。

### 第三步：保存管理员 API Key

把下面命令中的内容替换为刚才生成的管理员 API Key：

```bash
printf '%s' '在这里填写管理员 API Key' > quota-sync/secrets/sub2api_admin_api_key
chmod 600 quota-sync/secrets/sub2api_admin_api_key
```

### 第四步：修改现有 docker-compose.yml

把下面的 `quota-sync` 服务加入现有 `services:` 中。不要再写第二个 `services:`。

```yaml
services:
  quota-sync:
    image: wellblink/sub2api-autoreset:latest
    container_name: sub2api-quota-sync
    restart: unless-stopped
    read_only: true
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    ports:
      - "127.0.0.1:${QUOTA_SYNC_PORT:-8091}:8090"
    volumes:
      - ./quota-sync/data:/data
    tmpfs:
      - /tmp:size=16m,mode=1777
    environment:
      TZ: ${TZ:-Asia/Shanghai}
      SUB2API_BASE_URL: http://sub2api:8080/api/v1
      SUB2API_ADMIN_API_KEY_FILE: /run/secrets/sub2api_admin_api_key
      QUOTA_SYNC_LISTEN: ":8090"
      QUOTA_SYNC_STATE_PATH: /data/state.json
      DATABASE_HOST: postgres
      DATABASE_PORT: "5432"
      DATABASE_USER: ${POSTGRES_USER:-sub2api}
      DATABASE_PASSWORD: ${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
      DATABASE_DBNAME: ${POSTGRES_DB:-sub2api}
      DATABASE_SSLMODE: disable
      REDIS_HOST: redis
      REDIS_PORT: "6379"
      REDIS_USERNAME: ${REDIS_USERNAME:-}
      REDIS_PASSWORD: ${REDIS_PASSWORD:-}
      REDIS_DB: ${REDIS_DB:-0}
      REDIS_ENABLE_TLS: ${REDIS_ENABLE_TLS:-false}
    secrets:
      - sub2api_admin_api_key
    depends_on:
      sub2api:
        condition: service_healthy
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    networks:
      - sub2api-network
    healthcheck:
      test: ["CMD", "/usr/local/bin/quota-sync", "-healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

然后把下面的内容加入 Compose 文件最外层，和 `services:`、`networks:` 同级：

```yaml
secrets:
  sub2api_admin_api_key:
    file: ./quota-sync/secrets/sub2api_admin_api_key
```

项目中也提供了可以复制的示例文件：[deploy/compose-service.example.yml](deploy/compose-service.example.yml)。

### 第五步：核对名称和密码变量

启动前重点检查下面几项：

- 如果 Sub2API 服务不叫 `sub2api`，修改 `SUB2API_BASE_URL` 和 `depends_on`。
- 如果 PostgreSQL 服务不叫 `postgres`，修改 `DATABASE_HOST` 和 `depends_on`。
- 如果 Redis 服务不叫 `redis`，修改 `REDIS_HOST` 和 `depends_on`。
- 如果网络不叫 `sub2api-network`，修改 `networks`。
- `DATABASE_PASSWORD` 必须与 Sub2API 使用的 PostgreSQL 密码相同。
- `REDIS_PASSWORD` 必须与 Sub2API 使用的 Redis 密码相同。
- `TZ` 应与 Sub2API 保持一致，否则日额度的日期边界可能不一致。

可以先检查 Compose 是否能正常解析：

```bash
docker compose config >/dev/null
```

没有报错再继续启动。

### 第六步：启动服务

```bash
docker compose pull quota-sync
docker compose up -d --no-deps quota-sync
```

查看状态：

```bash
docker compose ps quota-sync
docker compose logs --tail=100 quota-sync
```

正常情况下，容器状态会显示为 `healthy`。

也可以从服务器本机检查：

```bash
curl http://127.0.0.1:8091/healthz
```

返回下面的内容就表示服务已经启动：

```json
{"status":"ok"}
```

## 方式二：从 Docker Hub 手动拉取镜像

如果想先把镜像下载到服务器，可以执行：

```bash
docker pull wellblink/sub2api-autoreset:latest
```

也可以固定使用某个版本，避免 `latest` 更新后发生变化：

```bash
docker pull wellblink/sub2api-autoreset:1.0.0
```

然后把 Compose 中的镜像改成对应版本：

```yaml
image: wellblink/sub2api-autoreset:1.0.0
```

镜像支持：

- `linux/amd64`
- `linux/arm64`

不建议用一条很长的 `docker run` 命令运行本项目，因为它需要同时连接 Sub2API、PostgreSQL、Redis、数据目录和 Docker secret。即使手动拉取镜像，也建议继续使用上面的 Compose 配置启动。

Docker Hub 地址：<https://hub.docker.com/r/wellblink/sub2api-autoreset>

## 方式三：从 GitHub 下载源码并构建

只有需要修改代码或自行构建镜像时，才需要下载 GitHub 源码。

```bash
git clone https://github.com/wellblink/Sub2API-AutoReset.git quota-sync
cd quota-sync
```

构建本地镜像：

```bash
docker build -t sub2api-autoreset:local .
```

Dockerfile 在构建过程中会自动运行全部 Go 测试。构建完成后，可以将 Compose 中的镜像改成自己的本地标签：

```yaml
image: sub2api-autoreset:local
```

如果希望由 Compose 直接构建，请把 `image` 改为：

```yaml
build:
  context: ./quota-sync
  dockerfile: Dockerfile
```

源码地址：<https://github.com/wellblink/Sub2API-AutoReset>

## 把管理页面嵌入 Sub2API

自动重置页面需要使用当前登录的 Sub2API 管理员身份。不要把容器的 8090 端口直接开放到公网，应通过 Sub2API 使用的同一个域名转发 `/quota-sync/`。

下面以宿主机 Nginx 为例。

### 第一步：添加 Nginx 转发

在 Sub2API 对应的 Nginx `server` 配置中加入：

```nginx
location /quota-sync/ {
    proxy_pass http://127.0.0.1:8091/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 180s;
    proxy_buffering off;
}
```

在原来代理 Sub2API 前端的 `location /` 中加入：

```nginx
proxy_set_header Accept-Encoding "";
sub_filter_once on;
sub_filter '</head>' '<script defer src="/quota-sync/menu.js"></script></head>';
```

保存后检查并重新加载 Nginx：

```bash
nginx -t && systemctl reload nginx
```

如果 Nginx 也运行在 Docker 中，`127.0.0.1:8091` 通常无法直接访问宿主机，需要改成 Nginx 容器能够访问的地址。

完整示例位于 [deploy/nginx-location.example.conf](deploy/nginx-location.example.conf)。

### 第二步：创建 Sub2API 自定义页面

在 Sub2API 管理后台创建一个自定义页面：

- 页面标识：`quota-auto-reset`
- 页面标题：`自动重置`
- 菜单地址：`/custom/quota-auto-reset`

页面内容填写：

```html
<iframe
  src="/quota-sync/embed"
  title="自动重置"
  frameborder="0"
  style="display:block;width:100%;height:calc(100vh - 208px);min-height:640px;border:0;border-radius:12px;background:transparent"
></iframe>
```

也可以直接复制 [deploy/quota-auto-reset.md](deploy/quota-auto-reset.md)。

Nginx 注入的菜单脚本会尝试把“自动重置”移动到“账号管理”下面。如果以后 Sub2API 修改了左侧菜单结构，自动定位可能失效，但自定义菜单本身仍会保留。

## 第一次使用

服务和页面都配置好后，按下面的顺序操作：

1. 使用管理员账号登录 Sub2API。
2. 打开左侧的“自动重置”。
3. 进入“账号映射”。
4. 选择需要监听的 OpenAI OAuth 上游账号。
5. 选择这个上游需要联动的下游订阅。
6. 选择需要重置的日、周、月额度。
7. 保存账号映射。
8. 回到“概览与设置”，确认轮询周期等参数。
9. 点击“立即检查”，确认账号和用量读取正常。
10. 检查没有错误后，再打开自动监听并保存。

每个标签页只保存当前标签页的设置。如果修改后没有保存就切换到其他标签，再回来时会恢复上次已经保存的内容。

## 检测设置说明

- **轮询周期（秒）**：两轮自动检查之间的基础间隔。系统会在后台加入随机偏移。
- **二次确认延迟（秒）**：发现用量下降后，等待多久再查一次。
- **自然重置保护窗口（秒）**：接近正常重置时间时不执行联动。
- **样本最大年龄（秒）**：Sub2API 已保存的用量超过这个时间后，才会强制查询上游。

建议先使用默认值运行一段时间，不要一开始就把轮询周期设得很短。

## 系统如何判断官方重置

每轮检查会读取同一个 7 天额度周期中的真实累计数据：

- 请求数
- Token 数
- 上游金额
- 标准金额
- 用户金额

其中任意一个累计值比上次小，才会进入二次确认。二次确认前还会检查：

- 是否接近自然重置时间；
- 重置卡数量是否减少；
- 原来的重置卡明细是否消失或被其他卡替换；
- 上游账号和下游订阅是否仍属于保存时的分组；
- 同一个上游额度周期是否已经联动过。

只要有一项无法确认，系统就不会重置下游额度。

### 这种检测有一个限制

假设官方重置发生在两次检查之间，同时用户又产生了大量新用量。如果新用量已经超过重置前的累计值，第二次看到的数据就不会下降，系统无法判断中间发生过重置。

缩短轮询周期可以降低这种情况出现的概率，但也会增加上游查询次数。

## 通知设置

支持：

- Bark
- 企业微信群机器人 Webhook

可以分别接收三类通知：

1. 检测到上游官方重置；
2. 已执行下游额度重置；
3. 某个下游用户的额度重置已回滚。

通知标题和正文都可以自定义。页面会列出可用的模板变量。

Bark Device Key 和企业微信 Webhook 会保存在 `quota-sync/data/state.json` 中。页面只显示脱敏后的开头和结尾，不会返回完整内容。

## 撤销说明

每个成功重置的下游订阅，可以在 24 小时内单独撤销一次。撤销时会保留重置以后新产生的消费。

- 如果仍是同一天，日用量会恢复为“重置前用量 + 重置后新增用量”。
- 如果已经跨天，昨天的日用量不会加到今天，今天的日用量保持不变。
- 如果原来的周额度或月额度周期仍然有效，周/月用量会恢复为“重置前用量 + 重置后新增用量”。
- 如果某个额度周期已经正常到期，只跳过这个周期，不影响其他周期。
- 如果期间又发生过其他重置，导致额度周期起始时间发生变化，系统会拒绝撤销，避免覆盖新数据。

撤销功能会直接访问 Sub2API 的 PostgreSQL 和 Redis，所以数据库、Redis 和时区设置必须与 Sub2API 完全一致。

## 更新方法

更新自动重置服务：

```bash
docker compose pull quota-sync
docker compose up -d --no-deps quota-sync
```

更新 Sub2API：

```bash
docker compose pull sub2api
docker compose up -d --no-deps sub2api
```

两者是不同的容器，正常更新 Sub2API 不会覆盖自动重置服务。

更新前建议备份：

```bash
cp -a quota-sync/data "quota-sync/data.backup.$(date +%Y%m%d-%H%M%S)"
```

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `TZ` | `Asia/Shanghai` | 时区，应与 Sub2API 相同 |
| `SUB2API_BASE_URL` | `http://sub2api:8080/api/v1` | Sub2API 容器内管理接口地址 |
| `SUB2API_ADMIN_API_KEY_FILE` | `/run/secrets/sub2api_admin_api_key` | 管理员 API Key 文件 |
| `QUOTA_SYNC_LISTEN` | `:8090` | 容器内监听地址 |
| `QUOTA_SYNC_STATE_PATH` | `/data/state.json` | 设置和运行记录文件 |
| `DATABASE_HOST` | `postgres` | PostgreSQL 服务名或地址 |
| `DATABASE_PORT` | `5432` | PostgreSQL 端口 |
| `DATABASE_USER` | `sub2api` | PostgreSQL 用户名 |
| `DATABASE_PASSWORD` | 空 | PostgreSQL 密码 |
| `DATABASE_DBNAME` | `sub2api` | PostgreSQL 数据库名 |
| `DATABASE_SSLMODE` | `disable` | PostgreSQL TLS 设置 |
| `REDIS_HOST` | `redis` | Redis 服务名或地址 |
| `REDIS_PORT` | `6379` | Redis 端口 |
| `REDIS_USERNAME` | 空 | Redis 用户名 |
| `REDIS_PASSWORD` | 空 | Redis 密码 |
| `REDIS_DB` | `0` | Redis 数据库编号 |
| `REDIS_ENABLE_TLS` | `false` | 是否使用 Redis TLS |

## 常见问题

### 页面提示 401

确认浏览器已经登录 Sub2API 管理员账号，并确认 `/quota-sync/` 与 Sub2API 使用完全相同的域名、协议和端口。

### 容器无法连接 Sub2API、PostgreSQL 或 Redis

先检查 Compose 中的服务名和网络名。自动重置服务必须与这三个服务在同一个 Docker 网络中。

查看日志：

```bash
docker compose logs --tail=200 quota-sync
```

测试 Sub2API 容器地址：

```bash
docker compose exec quota-sync wget -qO- http://sub2api:8080/health
```

### 页面没有可映射的上游账号

只有满足以下条件的账号才会显示：

- 平台是 OpenAI；
- 登录方式是 OAuth；
- 账号状态有效；
- 账号所在分组中存在有效的下游订阅。

### 检查成功，但一直没有联动

第一次自动检查只会保存比较基准，不会重置下游。

另外，请先在 Sub2API 账号管理中点击一次“次数”，让 Sub2API 保存重置卡数量和每张卡的到期时间。重置卡信息不完整时，系统不会执行联动。

### 上游已经被官方重置，但系统没有检测到

如果两次检查之间产生的新用量已经超过重置前用量，最终累计值不会下降，系统就无法确认发生过官方重置。可以适当缩短轮询周期。

## 数据和安全

- 不要把 8090 或宿主机的 8091 端口直接开放到公网。
- 管理员 API Key 应使用 Docker secret，不要直接写进 Compose。
- `quota-sync/data/state.json` 包含账号映射、通知凭据和运行记录，请限制文件权限。
- 不要把 `.env`、`data/`、`secrets/`、数据库备份或日志提交到 Git。
- 首次开启自动监听前，请再次确认上游账号和下游订阅映射。

## 免责声明

本项目是社区项目，不是 Sub2API 或 OpenAI 官方组件。Sub2API 的接口或数据库结构发生变化后，本项目可能需要同步更新。请先在测试环境验证，并自行承担自动重置和额度回滚带来的风险。

## 许可证

[MIT License](LICENSE)
