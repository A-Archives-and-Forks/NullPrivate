# NullPrivate

> Available in other languages: [العربية](./readmes/readme.ar-sa.md), [Deutsch](./readmes/readme.de-de.md), [Español](./readmes/readme.es-es.md), [Français (Canada)](./readmes/readme.fr-ca.md), [Français (France)](./readmes/readme.fr-fr.md), [日本語](./readmes/readme.ja-jp.md), [한국어](./readmes/readme.ko-kr.md), [Português (Brasil)](./readmes/readme.pt-br.md), [Русский](./readmes/readme.ru-ru.md), [简体中文](./readmes/readme.zh-cn.md), [繁體中文 (香港)](./readmes/readme.zh-hk.md)

NullPrivate is a fork of _AdGuardHome_, designed to provide a SaaS-hosted version with enhanced features and customizability. It is hosted on [Null Private](https://nullprivate.com).

## Key Features

### Original Features

1. **Network-Wide Ad Blocking**

   - Blocks ads and trackers across all devices in your network.
   - Operates as a DNS server that re-routes tracking domains to a “black hole.”

2. **Custom Filtering Rules**

   - Add your own custom filtering rules.
   - Monitor and control network activity.

3. **Encrypted DNS Support**

   - Supports DNS-over-HTTPS, DNS-over-TLS, and DNSCrypt.

4. **Built-in DHCP Server**

   - Provides DHCP server functionality out-of-the-box.

5. **Per-Client Configuration**

   - Configure settings for individual devices.

6. **Parental Control**

   - Blocks adult domains and enforces Safe Search on search engines.

7. **Cross-Platform Compatibility**

   - Runs on Linux, macOS, Windows, and more.

8. **Privacy-Focused**
   - Does not collect usage statistics or send data unless explicitly configured.

### New Features by NullPrivate

1. **DNS Routing with Rule Lists**

   - Customize DNS routing using rule lists defined in the configuration file.
   - Supports third-party rules like [Loyalsoldier/v2ray-rules-dat](https://github.com/Loyalsoldier/v2ray-rules-dat).

2. **Application-Specific Blocking Rule Lists**

   - Configure blocking of sources from specific applications.
   - Supports third-party configurations for flexible management.

3. **Dynamic DNS (DDNS)**

   - Provides dynamic domain name resolution capabilities for various scenarios.

4. **Advanced Rate Limiting**

   - Implements efficient traffic management and control measures.

5. **Enhanced Deployment Features**
   - Load balancing support.
   - Automatic certificate maintenance.
   - Optimized network connections.

For detailed documentation, visit: [NullPrivate Documentation](https://nullprivate.com/docs/)

## How to Use

### Download Binary

You can download the binary directly from the [Releases](https://github.com/NullPrivate/NullPrivate/releases) page. Once downloaded, follow these steps to run it:

```bash
./NullPrivate --web-addr 0.0.0.0:3000 --local-frontend --no-check-update --verbose
```

### Use Docker Image

Alternatively, you can use the Docker image available on [Docker Hub](https://hub.docker.com/r/nullprivate/nullprivate):

```bash
docker run --name NullPrivate -p 3000:3000 -p 80:80 -p 53:53/udp -v ./data/container/work:/opt/adguardhome/work -v ./data/container/conf:/opt/adguardhome/conf nullprivate/nullprivate:latest
```

## PostgreSQL Config Store

NullPrivate can store its configuration in PostgreSQL instead of only using the local `AdGuardHome.yaml` file.

- Enable it with `NULLPRIVATE_CONFIG_POSTGRES_ENABLED=true`
- Provide the connection string with `NULLPRIVATE_CONFIG_POSTGRES_DSN`
- When PostgreSQL is enabled and the database is empty, NullPrivate imports the local YAML configuration once if the configured file exists
- After the import, runtime reads and writes use PostgreSQL only

### Environment Variables

```bash
export NULLPRIVATE_CONFIG_POSTGRES_ENABLED=true
export NULLPRIVATE_CONFIG_POSTGRES_DSN='postgres://nullprivate:secret@127.0.0.1:5432/nullprivate?sslmode=disable'
```

### systemd Example

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

### Docker Example

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

### docker-compose Example

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

### Notes

- Keep the `-c` path and the config volume if you want one-time bootstrap import from an existing `AdGuardHome.yaml`
- If PostgreSQL is enabled and the database already has config, the local YAML file is ignored for runtime reads and writes
- If PostgreSQL is enabled but the database is empty and no local YAML file exists, NullPrivate starts in first-run mode as usual
