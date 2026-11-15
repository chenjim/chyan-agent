# A12 性能实测记录

> 对应 [requirements.md](requirements.md) §3 非功能需求与 §4 A12 验收项。
> 复现：`bash scripts/bench.sh 10`（脚本见 [scripts/bench.sh](../scripts/bench.sh)，摘要写 `dist/bench.json`）。

## 结论（2026-11-15 实测）

| 指标 | 阈值 | 实测 | 结果 |
|------|------|------|------|
| CLI 冷启动（`chyan version`，独立进程） | < 300ms | 中位 **17ms**（min 15 / max 32） | PASS |
| daemon 空闲 RSS（启动后稳态） | < 80MB | **37.7MB** | PASS |
| 六目标交叉编译 | 全部成功 | 6/6 产物，格式校验通过 | PASS |
| 宿主目标真跑 | 可运行 | `linux/amd64` 产物输出 `chyan 0.1.0-dev` | PASS |
| 遥测/上报代码 | 无 | 未发现 sentry/posthog/OTLP 等 SDK | PASS |

## 环境

| 项 | 值 |
|----|----|
| 主机 | Linux 6.18.18.c1186-trim x86_64 |
| CPU | 12th Gen Intel(R) Core(TM) i5-1240P（16 线程） |
| Go | go1.24.0 linux/amd64 |
| 构建口径 | `CGO_ENABLED=0 go build -trimpath`（与 `scripts/release.sh` 一致） |
| 数据目录 | 临时目录（`~/.agents/skills` 仍读本地技能库，与日常运行一致） |
| 说明 | 测量时工具链为 go1.24.0；当前 `go.mod` 要求 go 1.26（Go 自动切换工具链），冷启动数字仅作量级参考 |

## CLI 冷启动

`chyan version` 每次新进程，`CHYAN_HOME` 指向临时目录，10 次采样（毫秒）：

```
17 17 15 16 16 22 32 23 17 15   → min 15 / median 17 / max 32
```

冷启动只做配置定位与版本打印，不建库、不连 LLM；32ms 的离群值是调度抖动。

## daemon 空闲 RSS

`chyan serve --token bench`，等 `/api/v1/status` 可用后停留 3s 取 `ps -o rss`：

```
pid=761431  rss=38572KB (37.7MB)
```

同时检查空闲期连接：`ss -tnp` 按 pid 过滤后**无任何 established 连接**（仅有本机监听），
符合"无后台外发"。37.7MB 已包含本地技能库的加载开销。

## 交叉编译矩阵

| 产物 | 体积 | 格式 |
|------|------|------|
| linux/amd64 | 33M | ELF 64-bit LSB executable, x86-64, static |
| linux/arm64 | 31M | ELF 64-bit LSB executable, ARM aarch64, static |
| windows/amd64 | 33M | PE32+ console, x86-64 |
| windows/arm64 | 31M | PE32+ console, Aarch64 |
| darwin/amd64 | 33M | Mach-O 64-bit x86_64 |
| darwin/arm64 | 32M | Mach-O 64-bit arm64 |

静态链接（`CGO_ENABLED=0` + 纯 Go SQLite），产物无外部运行时依赖。

## 限制与后续

1. **跨平台运行未真机验证**：除 `linux/amd64` 实际执行外，其余 5 个产物只做了格式校验。
   A12 的"三平台产物可运行"还需要一台 Windows 与一台 macOS（或 VM）各跑一次 `chyan version`
   与 `chyan serve --token` 冒烟；Windows 按 §3 口径只验 TCP 本机回环面（无 unix socket）。
2. **未覆盖并发压测**：本次只测空闲内存与冷启动，未测 4 任务并发下的 RSS 峰值。
   如需补，用真实模型端点起 4 个任务并采样 `ps -o rss`。
3. **打包与校验值属 A8**：`scripts/release.sh` 负责六目标打包 + `SHA256SUMS`，本次未重复执行。
