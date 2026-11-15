# chyan 远程接入部署（P3-5）

> 状态：P3（2026-10-16）
> 适用：手机 PWA / 远程 TUI 访问家中/办公主机的 chyan daemon。
> 架构依据：architecture.md §4.5（双监听面）。

## 1. 监听面模型

| 面 | 默认 | 鉴权 | 用途 |
|----|------|------|------|
| 本地 | unix socket `~/.chyan/chyan.sock` | OS 用户权限 | CLI/TUI 本机访问，零端口占用 |
| 远程 | TCP `127.0.0.1:31415` | Bearer token（`--token` 或 `CHYAN_TOKEN`） | PWA/远程 TUI |

- 未设 token 时**远程面不启用**（安全默认）。
- token 由 `--token` / `CHYAN_TOKEN` 提供，chyan 自身**不落盘**；不要写进 shell history 或共享配置。
- 端口 31415 仅绑定 `127.0.0.1`，不直接暴露公网。

## 2. 推荐通道：Tailscale（首选）

Tailscale 提供 WireGuard 级加密 + 设备级身份，无需公网 IP、无需证书管理。

```bash
# 主机端
tailscale up                      # 加入 tailnet
chyan serve --token "$(openssl rand -hex 32)"
tailscale serve --bg 31415        # Tailscale HTTPS 终止（可选，给 PWA 一个 https 源）

# 手机端
# 1. 安装 Tailscale App 并登录同一账号
# 2. 浏览器打开 https://<主机名>.<tailnet>.ts.net/
# 3. PWA 登录页粘贴 token
```

要点：
- tailnet 内设备互访不经过公网；主机名走 MagicDNS。
- `tailscale serve` 自动签发 HTTPS 证书，PWA 的 Service Worker 需要 HTTPS（或 localhost）。
- 不用 `tailscale serve` 也可：手机直接 `http://<主机名>:31415`（tailnet 内明文，风险可接受）。

## 3. 备选通道：nginx 反代 + token

已有公网入口/域名时使用。反代终止 TLS，仅转发到本机 31415。

```nginx
server {
    listen 443 ssl http2;
    server_name chyan.example.com;

    ssl_certificate     /etc/letsencrypt/live/chyan.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/chyan.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:31415;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        # SSE 需要关闭缓冲，否则事件流被攒批
        proxy_buffering off;
        proxy_read_timeout 1h;
        proxy_set_header Connection "";
    }
}
```

要点：
- **SSE 特殊处理**：`proxy_buffering off` + 长 `proxy_read_timeout`，否则手机端收不到实时事件。
- 公网暴露时 token 是唯一防线，必须长随机（≥32 字节）+ 定期轮换。
- 建议叠加 IP 白名单（`allow/deny`）或 Basic Auth 双层。

## 4. Token 管理

chyan 不写 token；是否由部署方持久化（systemd EnvironmentFile、密码管理器等）由你决定。
下面示例把 token 写进 0600 的环境文件，避免出现在 shell history：

```bash
# 生成并写入 systemd 环境变量文件（权限 0600）
umask 077
printf 'CHYAN_TOKEN=%s\n' "$(openssl rand -hex 32)" > ~/.chyan/token.env
chmod 600 ~/.chyan/token.env

# systemd unit 里引用：EnvironmentFile=%h/.chyan/token.env
# PWA 登录页粘贴同一 token；换机/泄露时重新生成并更新两侧
```

## 5. 常见故障

| 症状 | 原因 | 处置 |
|------|------|------|
| PWA 连上但无实时事件 | 反代缓冲 SSE | `proxy_buffering off` |
| 401 Unauthorized | token 不一致 | 核对 `CHYAN_TOKEN` 与 PWA 登录值 |
| 连不上 31415 | 未设 token 时远程面关闭 | 启动时带 `--token` |
| Tailscale 主机名解析失败 | MagicDNS 未开 | 控制台开启 MagicDNS |

## 6. 安全边界

- 威胁模型（D20）：防误提交/误同步，不防本机同用户进程。
- token 泄露 = daemon 全权访问（可发起任务、读工作目录文件）。轮换即止血。
- PWA 为纯浏览器 SPA，无本地持久能力；退出登录即清 token（localStorage）。
