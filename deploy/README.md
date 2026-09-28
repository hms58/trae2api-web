# 一键部署

服务器上只需要三个文件（都在本目录）：

```bash
compose.yml     # 服务定义
.env            # 你的 API Key（从 .env.example 复制后修改）
README.md       # 本说明
```

## 部署

```bash
# 1. 建目录并进入
mkdir -p /opt/trae2api && cd /opt/trae2api

# 2. 把 compose.yml 和 .env.example 传到这个目录（scp / sftp 均可）
#    或者直接从 Gitee 下载这两个文件：
curl -fLO https://github.com/qiyuess/trae2api-web/raw/main/deploy/compose.yml
curl -fLO https://github.com/qiyuess/trae2api-web/raw/main/deploy/.env.example

# 3. 生成 .env（随机 32 字节 key，别用 changeme）
cp .env.example .env
sed -i "s/^TW2A_API_KEY=.*/TW2A_API_KEY=$(openssl rand -hex 32)/" .env
cat .env   # 记下这个 key，管理面板和 API 客户端都要用

# 4. 启动
docker compose up -d
```

## 验证

```bash
docker compose ps          # STATUS 应为 healthy
curl http://127.0.0.1:7864/healthz
```

浏览器打开 `http://<服务器IP>:7864/admin`，顶部输入 `.env` 里的 key 即可管理账号。

## 更新

```bash
docker compose pull
docker compose up -d
```

## 数据位置

账号凭证存放在 `./auths/`，账号池状态存放在 `./data/`，均在宿主机，重建容器不丢失。
