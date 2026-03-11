# NullPrivate

NullPrivate 是 _AdGuardHome_ 的一个分支，旨在提供一个具有增强功能和可定制性的 SaaS 托管版本。它托管在 [Null Private](https://nullprivate.com)。

## 主要功能

### 原始功能

1. **网络范围广告屏蔽**

   - 在您的网络中跨所有设备屏蔽广告和跟踪器。
   - 作为一个 DNS 服务器运行，将跟踪域名重新路由到“黑洞”。

2. **自定义过滤规则**

   - 添加您自己的自定义过滤规则。
   - 监控和控制网络活动。

3. **加密 DNS 支持**

   - 支持 DNS-over-HTTPS、DNS-over-TLS 和 DNSCrypt。

4. **内置 DHCP 服务器**

   - 开箱即用提供 DHCP 服务器功能。

5. **每客户端配置**

   - 为单个设备配置设置。

6. **家长控制**

   - 屏蔽成人域名并在搜索引擎上强制启用安全搜索。

7. **跨平台兼容性**

   - 在 Linux、macOS、Windows 等系统上运行。

8. **注重隐私**
   - 除非明确配置，否则不收集使用统计数据或发送数据。

### NullPrivate 新增功能

1. **使用规则列表的 DNS 路由**

   - 使用配置文件中定义的规则列表自定义 DNS 路由。
   - 支持第三方规则，如 [Loyalsoldier/v2ray-rules-dat](https://github.com/Loyalsoldier/v2ray-rules-dat)。

2. **特定应用的屏蔽规则列表**

   - 配置从特定应用程序的源进行屏蔽。
   - 支持第三方配置以实现灵活管理。

3. **动态 DNS (DDNS)**

   - 为各种场景提供动态域名解析能力。

4. **高级速率限制**

   - 实施高效的流量管理和控制措施。

5. **增强的部署功能**
   - 支持负载均衡。
   - 自动证书维护。
   - 优化网络连接。

有关详细文档，请访问：[NullPrivate 文档](https://nullprivate.com/docs/)

## 使用方法

### 下载二进制文件

您可以从 [Releases](https://github.com/NullPrivate/NullPrivate/releases) 页面直接下载二进制文件。下载后，按照以下步骤运行：

```bash
./NullPrivate -c ./AdGuardHome.yaml -w ./data --web-addr 0.0.0.0:34020 --local-frontend --no-check-update --verbose
```

### 使用 Docker 镜像

或者，您可以使用 [Docker Hub](https://hub.docker.com/repository/docker/nullprivate/nullprivate) 上可用的 Docker 镜像：

```bash
docker run --rm --name NullPrivate -p 34020:80 -v ./data/container/work:/opt/adguardhome/work -v ./data/container/conf:/opt/adguardhome/conf nullprivate/nullprivate:latest
```

## 使用 PostgreSQL 存储配置

NullPrivate 现在支持将配置存储到 PostgreSQL，而不是仅依赖本地 `AdGuardHome.yaml` 文件。

- 使用 `NULLPRIVATE_CONFIG_POSTGRES_ENABLED=true` 开启 PostgreSQL 配置存储
- 使用 `NULLPRIVATE_CONFIG_POSTGRES_DSN` 提供连接串
- 当 PostgreSQL 已启用且数据库为空时，如果本地配置文件存在，会在首次启动时自动导入一次
- 导入完成后，运行期配置读写将只使用 PostgreSQL

### 环境变量

```bash
export NULLPRIVATE_CONFIG_POSTGRES_ENABLED=true
export NULLPRIVATE_CONFIG_POSTGRES_DSN='postgres://nullprivate:secret@127.0.0.1:5432/nullprivate?sslmode=disable'
```

### systemd 示例

```ini
[Unit]
Description=NullPrivate
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/nullprivate
ExecStart=/opt/nullprivate/NullPrivate -c /opt/nullprivate/conf/AdGuardHome.yaml -w /opt/nullprivate/data --web-addr 0.0.0.0:3000 --no-check-update
Environment="NULLPRIVATE_CONFIG_POSTGRES_ENABLED=true"
Environment="NULLPRIVATE_CONFIG_POSTGRES_DSN=postgres://nullprivate:secret@127.0.0.1:5432/nullprivate?sslmode=disable"
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

### Docker 示例

```bash
docker run --rm --name NullPrivate \
  -p 3000:3000 \
  -p 80:80 \
  -p 53:53/udp \
  -e NULLPRIVATE_CONFIG_POSTGRES_ENABLED=true \
  -e NULLPRIVATE_CONFIG_POSTGRES_DSN='postgres://nullprivate:secret@postgres:5432/nullprivate?sslmode=disable' \
  -v ./data/container/work:/opt/adguardhome/work \
  -v ./data/container/conf:/opt/adguardhome/conf \
  nullprivate/nullprivate:latest
```

### docker-compose 示例

```yaml
services:
  nullprivate:
    image: nullprivate/nullprivate:latest
    container_name: NullPrivate
    restart: unless-stopped
    ports:
      - "3000:3000"
      - "80:80"
      - "53:53/udp"
    environment:
      NULLPRIVATE_CONFIG_POSTGRES_ENABLED: "true"
      NULLPRIVATE_CONFIG_POSTGRES_DSN: postgres://nullprivate:secret@postgres:5432/nullprivate?sslmode=disable
    volumes:
      - ./data/container/work:/opt/adguardhome/work
      - ./data/container/conf:/opt/adguardhome/conf
    depends_on:
      - postgres

  postgres:
    image: postgres:17
    restart: unless-stopped
    environment:
      POSTGRES_DB: nullprivate
      POSTGRES_USER: nullprivate
      POSTGRES_PASSWORD: secret
    volumes:
      - ./data/postgres:/var/lib/postgresql/data
```

### 说明

- 如果您希望从已有 `AdGuardHome.yaml` 做一次性导入，请保留 `-c` 参数以及配置文件挂载目录
- 如果 PostgreSQL 中已经存在配置，运行期将忽略本地 YAML 文件
- 如果 PostgreSQL 已启用，但数据库为空且本地 YAML 也不存在，则仍会进入首次安装向导流程
