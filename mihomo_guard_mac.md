# mi-guard 的 macOS 实现设计（v3）

项目名为 `mi-guard`，可执行文件为 `mihomo-guard`。本节是现行实现契约。文末保留原始 v2 草案，便于查阅方案演进。

## 业务与测速路径

| 路径 | 选择器 | 后续动作 |
| --- | --- | --- |
| dialer-proxy-1 | primary-group-1 | 验证链路与 Google 地区，尝试让 direct-group-1 跟随 |
| dialer-proxy-2 | primary-group-2 | 验证链路，尝试让 direct-group-2 跟随 |
| 独立手动组选路 | `mode: selector` 的 `name` | 只切该 select 组，用该组验证新连接的出口 |
| 候选下载测试 | 独立的 mi-guard-bench | 本地监听器直接引用此组。前置下载样本按 provider 和生效节点名归档 |

守护模式使用正在服务的 Mihomo 内核，`/version` 提供运行版本。已有 provider 的候选从控制器读取，保留主配置中的名字前缀与协议参数覆盖。临时手测新订阅时，显式的 `bench --source` 调用 `path_to_your_mihomo_core` 所指内核，使用临时目录、回环端口和仅含源节点的配置。具体配置及安装见 [README](README.md)。

默认配置提供两个中性的链式示例。用户可以按同样的方式增加其他链式前置，也可以用 `mode: selector` 单独维护普通手动选择组。地区命名仅作为可选的手动测速示例。各模式的必填项、地区监听绑定和完整示例见[随包默认配置](share/mi-guard/mihomo-guard-default.yaml)。

## 从观测到切换

1. 每 30 秒检测当前链。连续两次失败进入恢复，依次轻量验活同组备选，优先尝试有近期成功记录的节点。所有备选不可用时保留原选择。
2. 每 12 小时采样候选，每个节点最多 4 MiB。每 2 小时可对在用前置做 1 MiB 快筛。两个地域的直出组共享前置样本。手测可按组、节点或正则过滤。
3. `severe_drop_pct: 20` 表示**比此前的正常带宽中位数下降至少 20%**。基准 100 Mbps、当前 80 Mbps 时进入准备，80.01 Mbps 时继续留用。1 分钟后复测，默认需要两次连续降速证据。只有确认存在更好的可用备选才开始切换。
4. `near_best_pct: 20` 作用于同轮候选。达到本轮最快带宽的 80% 即进入优先档，比较应用层探测失败比例、历史较低分位带宽及当前选择。延迟用于进一步区分。同档宽容和相对自身降速各有独立基准。
5. 提交主组选择前回读当前节点。切换、定向清理旧连接、链路验活和可选地区检查完成后，才尽力同步直出组。候选未通过时尝试后续候选。严格地区模式失败回退。宽松模式可保留已验活的出口并报告地区异常。手动操作优先。
6. 每轮全组测试产生一条摘要。实际切换产生一条通知，记录主组的最终结果和直出跟随情况。通知由用户登录会话中的本地助手投递。

## 流量与历史

裁剪的下载器通过本机专用监听测试一个节点，默认单流。启用多条下载流时，所有流平分同一次节点的总额度，每条连接均核对线上实际链路。请求指定目标字节数，客户端以实际读取字节和墙钟时间计量。到达样本大小或时限时结束。部分读取失败照实计费并记录错误。确认样本上限为 8 MiB，默认账期下载响应体预算为 6 GiB。发请求前预留额度，进程异常退出时采用保守计费。

`speed-history.json` 使用临时文件与原子替换，每个 provider/节点保留最近 50 次带宽样本。记录时间、路径、类型、Mbps、字节、耗时、探测延迟和失败数/尝试数。链路心跳单独汇总。带宽预算按账期保存，旧样本移出窗口不会返还已经使用的流量。控制器、测速、历史访问均支持取消。

<details>
<summary>原始 v2 方案（历史记录）</summary>

# `mihomo-guard` 工具架构与详细设计文档（v2 · Mac 专用精简版）

> **文档定位**：本规格书面向 AI 编程助手与 Go 开发者，完整定义了 macOS 环境下配合 ClashX Meta（Mihomo 内核）运行的轻量级代理守护工具 `mihomo-guard` 的架构、接口、配置与验收标准，可直接作为编码实施依据。

---

## 1. 背景、目标与范围边界

### 1.1 业务痛点
在 macOS 长期以 Mihomo 作为主力代理内核的场景下，存在三类内核原生策略（`url-test` / `fallback`）无法同时解决的痛点：
1. **带宽与 IP 粘性矛盾**：美区（US）和新加坡（SG）出口均需依赖手工筛选的高带宽节点，并要求日常保持强 IP 粘性（不因延迟微幅波动频繁跳 IP 触发风控）；但机场单个节点偶发断连或晚高峰严重限速不可控。
2. **链式 WARP 的“假活送港”**：美区链式代理（`机场前置 -> dialer-proxy-1`）即使 204 连通正常，特定前置对应的 Cloudflare `/24` 出口池仍可能被 Google 动态判为香港/受限地区（导致搜索跳 `.hk`、AI Mode / Gemini 不可用）。
3. **链式切换后的 UDP 残留**：切换 `dialer-proxy` 上游节点后，WireGuard 已有连接可能继续死磕旧死链，需主动清理活跃连接以实现秒级恢复。

### 1.2 范围边界（Scope）
* **支持平台**：**仅限 macOS（Apple Silicon / `darwin/arm64`）**，放弃 OpenWrt 路由器端适配，彻底降低构建、内存与配置同步复杂度（路由器端由原生 Mihomo `fallback` 兜底）。
* **核心复用**：直接将 `github.com/faceair/clash-speedtest/speedtester` 作为 Go 依赖包引入，复用其并发下载测速能力，避免重复造轮子。
* **纳管目标**：聚焦 4 个核心出口策略组——`direct-group-1`、`direct-group-2`、`primary-group-1`、`primary-group-2`。

---

## 2. 架构决策推理与方案对比

### 2.1 分步架构推理
1. **为什么放弃双端架构后直接引入 `clash-speedtest`？**
   * `faceair/clash-speedtest` 在进程内嵌入了 Mihomo 内核适配器，可读取本地 YAML 配置发起多线程并发下载测速。虽然编译后二进制体积约 25–35 MB、测速峰值内存几十 MB，在路由器上无法落地，但在 Mac 上完全无感，且 8 个 US 候选节点 5–8 秒即可并发出榜。
2. **如何解决 Mac 开启 TUN 后的测速“回环套娃”？**
   * `speedtester` 会直接向机场节点发起外联；若不放行，流量会被 ClashX Meta 的系统级 TUN 再次劫持，导致测速失真或死循环。
   * **解法**：在 Mihomo `rules` 最顶部仅加 1 条 `- PROCESS-NAME,mihomo-guard,DIRECT`，把工具进程自身发出的物理节点测速流量直接放行到物理网卡。
3. **如何在不复制 WARP 私钥的前提下实现链路地区校验？**
   * `speedtester` 负责单跳机场节点的并发带宽排序，不负责经 `dialer-proxy-1` 链式出站后的 Google 地区校验。
   * **解法**：在 Mihomo 配置中仅增加一个 5 行 `listeners` 本地端口（`127.0.0.1:7898`），直接绑定到现成的 `dialer-proxy-1` 代理节点。该节点的 Mihomo `dialer-proxy` 设为 `primary-group-1`。触发故障切换时，工具先用 `speedtester` 并发算出带宽榜 `[N1, N2, ...]`，再按榜热切并经 `:7898` 逐一验证 `.google.com`，兼顾零配置冗余与端到端准确性。

### 2.2 架构方案优缺点对比

| 对比维度 | v2 采纳方案：Mac 专用版（集成 `clash-speedtest`） | 备选方案：纯 Mihomo API 隔离通道（兼容路由器） |
| :--- | :--- | :--- |
| **Mihomo 配置侵入度** | **极低**：仅加 1 条 `PROCESS-NAME` 规则 + 1 个 5 行 `:7898` 监听，无隐藏策略组、无重复私钥 | **较高**：需维护 2 个监听端口、2 个隐藏策略组及 1 份镜像 WARP 完整配置 |
| **全组测速效率** | **高（并发）**：`speedtester` 并发下载，8 节点约 5–8 秒出榜 | **较低（串行）**：共用隔离端口排队测速，约 25–35 秒 |
| **工程与维护复杂度** | **低**：单测速引擎、单架构编译、无构建标签与 OpenWrt `procd` 适配 | **高**：需抽象双引擎、跨平台交叉编译、兼顾 Nikki Mixin 同步 |
| **资源占用** | 二进制约 25–35 MB，测速峰值内存几十 MB（Mac 端无压力） | 二进制 < 8 MB，常驻内存 < 15 MB |
| **平台覆盖** | 仅 macOS（路由器改用原生 `fallback` 兜底） | macOS + OpenWrt 双端同源 |

---

## 3. Mihomo 侧最小前置配置

运行 `mihomo-guard` 前，需在 Mac 的 Mihomo 配置文件中补充以下两处增量（共 6 行有效配置）：

```yaml
# 1. 顶层新增：专供 mihomo-guard 直连校验 dialer-proxy-1 出口状态（绕过 rules）
listeners:
  - name: mi-guard-geo-in
    type: mixed
    port: 7898
    listen: 127.0.0.1
    proxy: dialer-proxy-1

# 2. rules 最顶部新增：放行本工具进程，避免 clash-speedtest 测速流量被 TUN 劫持套娃
rules:
  - PROCESS-NAME,mihomo-guard,DIRECT
```

---

## 4. 系统总体架构与状态机设计

### 4.1 模块交互拓扑

```text
+-------------------------------------------------------------------+
|                      mihomo-guard (macOS)                         |
|                                                                   |
|  [Scheduler] ---> [Per-Group Sticky State Machine]                |
|                     |                  |                 |        |
|           1. 轻保活 / 热切     2. 并发带宽测速    3. GeoIP 防送港   |
|           & 清理僵尸连接       (故障 / 低频抽检)   & CF Trace     |
|                     v                  v                 v        |
|             [Mihomo Client]    [Speedtest Wrapper]  [Geo Prober]  |
|            (127.0.0.1:9090)   (clash-speedtest包) (127.0.0.1:7898)|
+---------------------|------------------|-----------------|--------+
                      |                  |                 |
                      v                  v                 v
               ClashX Meta        直连机场物理节点     经 dialer-proxy-1
            (REST Control API)  (PROCESS-NAME直连)   校验 .google.com
```

### 4.2 四条核心设计原则
1. **强粘性优先（Sticky-First）**：只要当前活跃节点满足“存活 + 带宽达标 + 地区合规”，即便存在更快的候选节点也**绝不主动切换**。
2. **人工操作优先（Manual-Override-Respect）**：每轮巡检前先调 `GET /proxies/{group}` 读取 Mihomo 当前 `now` 节点。若 `now != state.LastKnownNode`，判定为用户在 UI 手动切线，立即接纳 `now` 为新粘性目标、清零失败计数并解除旧状态，绝不与用户抢权。
3. **防抖与冷静期（Debounce & Cooldown）**：单次探针失败只递增计数、不立即切换；连续达到 `fail_threshold`（默认 2 次）才触发 `FAILOVER`；切换完成后进入冷静期（默认 5 分钟），防止网络波动引发乒乓切换。
4. **全挂保护（All-Dead Guard）**：若全组测速结果全部超时（大概率为本地物理断网或机场全局故障），**保持原节点不动**，不乱切、不进冷静期，等待下个周期自动重试。

### 4.3 单策略组状态机

```text
                [启动 / 同步 Mihomo 当前 now 节点]
                                |
                                v
                     +---------------------+
        +----------->|       HEALTHY       |<-----------------------+
        |            |  (L1保活/L2地区/L3抽检) |                      |
        |            +---------------------+                        |
        |                       |                                   |
   探针恢复正常            单次失败 / GeoIP异常                       |
  (fail_count=0)                v                                   |
        |            +---------------------+                        |
        +------------|      DEGRADED       |                        |
                     |  fail_count++       |                        |
                     |  (未达阈值, 不切换)   |                        |
                     +---------------------+                        |
                                |                                   |
                   fail_count >= fail_threshold                     |
                   或 L3 带宽抽检 < min_speed_mbps                   |
                                v                                   |
                     +---------------------+                        |
                     |      FAILOVER       |                        |
                     | 1. 过滤黑名单与当前坏节点                       |
                     | 2. speedtester 并发出榜                      |
                     | 3. PUT 热切 + 清僵尸连接                      |
                     | 4. (若开启Geo) 按榜顺延验证 .google.com       |
                     +---------------------+                        |
                                |                                   |
                                v                                   |
                     +---------------------+                        |
                     |      COOLDOWN       |--- 冷静期满 (如 5m) ----+
                     | (仅记录, 禁止再次切线) |
                     +---------------------+
```

### 4.4 三层探针规格

| 探针层级 | 默认周期 | 作用范围 | 执行通道与判定标准 |
| :--- | :--- | :--- | :--- |
| **L1 存活探针** | `45s` | 仅当前活跃节点 | 调用 `GET /proxies/{target}/delay?url=https://www.gstatic.com/generate_204&timeout=3000`。对于 `primary-group-1` / `primary-group-2`，通过 `liveness_proxy_override` 直接探测 `dialer-proxy-1` / `dialer-proxy-2`，确保端到端隧道存活而非仅前置存活。 |
| **L2 防送港探针** | `5m` | 仅开启 `enable_geo_check` 的组 | 经 `geo_proxy_addr`（如 `socks5://127.0.0.1:7898`）GET 请求 `https://www.google.com/searchdomaincheck?format=domain`，`strings.TrimSpace` 后**严格等于 `.google.com`**；同时请求 `https://www.cloudflare.com/cdn-cgi/trace` 解析 `ip=`、`colo=`、`loc=` 打印进结构化日志。 |
| **L3 带宽抽检探针** | `30m` | 仅当前活跃节点 | 调用 `speedtester` 单独对当前 `now` 节点发起 1 次下载测速；仅当实测速率低于 `min_speed_mbps` 时立即转入 `FAILOVER` 触发全组并发重选。 |

### 4.5 故障切换与顺位防送港算法（Failover Flow）

1. **构建候选池**：从 Mihomo `GET /proxies/{group}` 读取 `all` 列表，过滤掉：
   * 非物理节点（`DIRECT`、`REJECT`、嵌套策略组）；
   * 命中 `exclude_regex` 的节点（如 `example-free`、`example-excluded`、过期/剩余流量提示行）；
   * 当前触发故障的坏节点（若过滤后列表为空则保留，防止单节点组无路可退）。
2. **并发带宽排行**：通过 `clash-speedtest/speedtester` 加载 `mihomo.config_path`，仅针对上述候选池执行并发下载测速，剔除测速失败/超时的节点，按下载速率（MB/s）降序排列（速率相近时按 TTFB 升序），产出有序榜单 `[N1, N2, ..., Nk]`。若榜单为空，触发“全挂保护”，维持原节点并结束本轮。
3. **无 Geo 校验组（如 `direct-group-2`、`primary-group-2`）**：
   * 直接调用 `PUT /proxies/{group}` 切换至 `N1`。
   * 若配置了 `cleanup_chain_targets`，立即清理僵尸连接。
   * 进入 `COOLDOWN`。
4. **开启 Geo 校验组（如 `primary-group-1`）**：
   * 顺序遍历榜单 `Ni`（`i = 1..k`）：
     1. 调用 `PUT /proxies/{group}` 切换至 `Ni`。
     2. 清理 `cleanup_chain_targets`（如 `dialer-proxy-1`）及旧节点关联的活跃连接，并等待 `300ms` 让新隧道就绪。
     3. 经 `geo_proxy_addr`（禁用 Keep-Alive）执行 L2 探针校验 `.google.com`。
     4. 若校验通过：记录 `ip`、`colo` 与实测带宽，锁定 `Ni` 为新粘性节点，进入 `COOLDOWN` 并返回。
     5. 若校验失败（送港）：打印 `WARN` 日志并继续尝试 `Ni+1`。
   * **兜底降级**：若所有存活候选均送港：
     * `strict_geo_fallback == false`（默认）：回切至带宽最高的存活节点 `N1` 并清连接，输出 `WARN` 告警（优先保通）。
     * `strict_geo_fallback == true`：回切至原节点，等待下个周期重试。

### 4.6 链式僵尸连接清理逻辑
当需要清理目标链（如 `["dialer-proxy-1"]`）时：
1. 调用 `GET /connections` 获取当前所有活跃连接。
2. 筛选 `conn.Chains` 中包含任意 `cleanup_chain_targets` 元素或包含被换下旧节点名称的连接 ID。
3. 并发调用 `DELETE /connections/{id}` 断开旧隧道，促使业务侧下一跳请求立即建立新隧道连接。

---

## 5. 工程目录结构与核心 Go 接口契约

### 5.1 推荐项目结构

```text
mihomo-guard/
├── cmd/
│   └── mihomo-guard/
│       └── main.go                   # CLI 入口 (daemon / bench / status)
├── internal/
│   ├── config/
│   │   └── config.go                 # YAML 解析、默认参数填充、路径展开 (~) 与校验
│   ├── mihomo/
│   │   ├── client.go                 # :9090 REST API 客户端 (PathEscape / Secret)
│   │   └── types.go                  # ProxyInfo / ConnectionsResp 模型
│   ├── tester/
│   │   ├── speedtest.go              # 封装 faceair/clash-speedtest/speedtester
│   │   └── geo.go                    # Google searchdomaincheck & CF trace 探针
│   ├── guard/
│   │   ├── manager.go                # 多组协程调度、优雅退出 (SIGINT/SIGTERM)
│   │   └── group.go                  # 单组状态机、人工干预同步、顺位切换逻辑
│   └── report/
│       └── table.go                  # bench / status 终端对齐表格打印
├── configs/
│   └── config.example.yaml           # 配置模板
├── deploy/
│   └── com.user.mihomo-guard.plist   # macOS launchd 常驻配置
├── Makefile                          # 本地编译与安装快捷命令
└── go.mod                            # Go 1.24+ (对齐 clash-speedtest 要求)
```

### 5.2 核心接口契约

```go
package mihomo

import (
	"context"
	"time"
)

type ProxyInfo struct {
	Name string   `json:"name"`
	Type string   `json:"type"`
	Now  string   `json:"now"`
	All  []string `json:"all"`
}

type Connection struct {
	ID     string   `json:"id"`
	Chains []string `json:"chains"`
}

type Client interface {
	GetProxy(ctx context.Context, name string) (*ProxyInfo, error)
	SelectProxy(ctx context.Context, group, node string) error
	TestDelay(ctx context.Context, node, testURL string, timeout time.Duration) (int, error)
	ListConnections(ctx context.Context) ([]Connection, error)
	CloseConnection(ctx context.Context, id string) error
}
```

```go
package tester

import (
	"context"
	"time"
)

type SpeedResult struct {
	NodeName  string
	Alive     bool
	TTFB      time.Duration
	SpeedMBps float64 // 实测有效下载速率 (MB/s)
	Err       error
}

type GeoResult struct {
	OK       bool
	Actual   string // 如 ".google.com" 或 ".google.com.hk"
	ExitIP   string // 来自 cdn-cgi/trace
	ExitColo string // 来自 cdn-cgi/trace (如 "LAX")
	ExitLoc  string // 来自 cdn-cgi/trace (如 "US")
	Err      error
}

type SpeedTester interface {
	// TestCandidates 加载本地 Mihomo 配置，仅对 candidates 列表执行并发下载测速，返回按速率降序排列的结果
	TestCandidates(ctx context.Context, candidates []string) ([]SpeedResult, error)
}

type GeoProber interface {
	// Check 经指定 socks5/http 代理地址校验 Google 地区与 CF Trace（每次禁用 Keep-Alive）
	Check(ctx context.Context, proxyAddr string) GeoResult
}
```

---

## 6. 配置文件完整 Schema（`config.yaml`）

```yaml
mihomo:
  api_url: "http://127.0.0.1:9090"
  secret: ""                         # 与 Mihomo 配置中的 secret 保持一致
  # 供 clash-speedtest/speedtester 加载订阅与节点的本地 Mihomo 配置文件路径（支持 ~ 展开）
  config_path: path_to_your_config.yaml
  http_timeout: 5s

speedtest:
  download_url: "https://speed.cloudflare.com/__down?bytes=10485760"
  download_bytes: 10485760           # 单次测速数据上限 10MB
  timeout: 6s                        # 单节点测速硬超时
  concurrent: 4                      # 并发协程数（建议 4，避免抢占家庭宽带导致读数失真）
  liveness_url: "https://www.gstatic.com/generate_204"
  liveness_timeout: 3s
  geo_check_url: "https://www.google.com/searchdomaincheck?format=domain"
  expected_geo: ".google.com"
  fetch_cf_trace: true

groups:
  # 1. 美区原生出口
  - name: direct-group-1
    enabled: true
    exclude_regex: "(?i)(example-free|剩余|到期|官网|DIRECT|REJECT)"
    liveness_interval: 45s
    bandwidth_check_interval: 30m
    enable_geo_check: false          # 若 💡 Google 改走原生美区，可按需开启并配 geo_proxy_addr
    min_speed_mbps: 5.0
    fail_threshold: 2
    cooldown: 5m
    cleanup_chain_targets: []

  # 2. 新加坡主力出口
  - name: direct-group-2
    enabled: true
    exclude_regex: "(?i)(example-free|剩余|到期|官网|DIRECT|REJECT)"
    liveness_interval: 45s
    bandwidth_check_interval: 30m
    enable_geo_check: false
    min_speed_mbps: 5.0
    fail_threshold: 2
    cooldown: 5m
    cleanup_chain_targets: []

  # 3. 链式第一组前置（端到端保活 + 强防送港 + 僵尸连接清理）
  - name: primary-group-1
    enabled: true
    liveness_proxy_override: dialer-proxy-1
    exclude_regex: "(?i)(example-free|example-excluded)"
    liveness_interval: 45s
    bandwidth_check_interval: 30m
    enable_geo_check: true
    geo_check_interval: 5m
    geo_proxy_addr: "socks5://127.0.0.1:7898"
    strict_geo_fallback: false
    min_speed_mbps: 3.0
    fail_threshold: 2
    cooldown: 5m
    cleanup_chain_targets: [dialer-proxy-1]

  # 4. 链式第二组前置
  - name: primary-group-2
    enabled: true
    liveness_proxy_override: dialer-proxy-2
    exclude_regex: "(?i)(example-free)"
    liveness_interval: 60s
    bandwidth_check_interval: 45m
    enable_geo_check: false
    min_speed_mbps: 3.0
    fail_threshold: 2
    cooldown: 5m
    cleanup_chain_targets: [dialer-proxy-2]

logging:
  level: "info"                      # debug | info | warn | error
  format: "text"                     # text | json
```

---

## 7. CLI 命令与 macOS 部署规范

### 7.1 CLI 子命令（使用 Go 标准库 `flag`）

| 命令 | 说明 | 示例 |
| :--- | :--- | :--- |
| `daemon` | 前台/守护常驻运行，按配置执行全组粘性保活、抽检与自动切线 | `./mihomo-guard daemon -c ~/.config/mihomo-guard/config.yaml` |
| `bench` | 手动对指定组（或全部启用组）执行并发带宽测速并输出排行表；带 `--apply` 自动热切至最优合规节点 | `./mihomo-guard bench -c config.yaml -g primary-group-1 --apply` |
| `status` | 读取并表格化展示各纳管组当前在 Mihomo 中的选中节点、当前延迟与（若开启 Geo）当前 Google 地区及 CF 落点 | `./mihomo-guard status -c config.yaml` |

### 7.2 构建与 `launchd` 常驻部署
* **编译命令**（必须命名为 `mihomo-guard` 以匹配 `PROCESS-NAME` 规则）：
  ```bash
  go build -trimpath -ldflags="-s -w" -o mihomo-guard ./cmd/mihomo-guard
  ```
* **macOS `launchd` 服务**（`~/Library/LaunchAgents/com.user.mihomo-guard.plist`）：
  * 设置 `RunAtLoad = true`、`KeepAlive = true`；
  * `StandardOutPath` 与 `StandardErrorPath` 指向 `~/Library/Logs/mihomo-guard.log`；
  * 二进制安装路径推荐 `/usr/local/bin/mihomo-guard` 或 `~/.local/bin/mihomo-guard`。

---

## 8. 实现防踩坑指引与验收标准

### 8.1 开发防踩坑清单（必读）
1. **先读依赖包真实签名再编码**：执行 `go get github.com/faceair/clash-speedtest@latest` 后，**务必先运行 `go doc github.com/faceair/clash-speedtest/speedtester`** 核对导出结构体（如 `Config`、`SpeedTester`、`Result`）的精确字段名与方法签名，严禁凭记忆编造字段。同时注意该库采用 **GPL-3.0** 协议（自用完全无问题，仓库内保留合规声明即可）。
2. **严禁仅用 `go run` 做端到端联调**：`go run` 编译出的临时可执行文件进程名并非 `mihomo-guard`，会被 ClashX Meta 的 TUN 劫持导致测速走代理套娃。联调与验收前必须先 `go build -o mihomo-guard ./cmd/mihomo-guard` 后直接执行二进制文件。
3. **强制 URL 路径编码**：策略组名和节点名可能包含 Emoji、中文及空格，拼接 `:9090` API 路径时必须全部经由 `url.PathEscape(name)` 处理。
4. **Geo 探针强制禁用连接复用**：`internal/tester/geo.go` 构造 `http.Client` 时必须设置 `Transport.DisableKeepAlives = true`（并在请求结束关闭 Body），防止热切前置节点后复用到旧 TCP/SOCKS5 连接导致读出过期地区结果。
5. **配置文件加载的相对路径处理**：`clash-speedtest` 解析 `config.yaml` 中的 `proxy-providers` 相对路径（如 `./proxy_providers/proxy-providers-1.yaml`）时，需注意其工作目录或 `ConfigPaths` 解析行为；确保无论在何处启动 `mihomo-guard`，都能正确解析与 Mihomo 主配置关联的订阅缓存文件。
6. **Mihomo 重启与断网容错**：当 `:9090` 返回 `connection refused` 或超时，严禁 `panic` 或退出进程，应输出 `WARN` 日志并指数退避（`5s -> 15s -> 30s`）等待 Mihomo 恢复。

### 8.2 验收测试用例（Acceptance Checklist）
1. **无套娃并发测速验收**：执行 `./mihomo-guard bench -c config.yaml -g primary-group-2`，同时在 ClashX Meta「连接」面板确认 `mihomo-guard` 进程的所有测速连接均命中 `DIRECT`，且终端 10 秒内打印完整按速率降序排列的表格。
2. **WARP 顺位防送港验收**：执行 `./mihomo-guard bench -c config.yaml -g primary-group-1 --apply`，确认工具切至实测最快且经 `:7898` 验证返回 `.google.com` 的前置节点，日志输出对应的 `ip` 与 `colo`，且旧 `dialer-proxy-1` 活跃连接被正确清理。
3. **人工干预尊重验收**：`daemon` 运行中，在 ClashX Meta 手动切换 `primary-group-2` 到另一个正常节点，观察日志记录 `manual_override_detected` 且守护进程接受新节点、不会将其改回。
4. **故障自愈验收**：临时将某组 `fail_threshold` 调为 `1` 并手动将其切至超时节点，确认 1 个检测周期内自动触发 `FAILOVER` 并发测速并热切回最优可用节点。
5. **优雅退出验收**：向运行中的 `daemon` 发送 `Ctrl+C`（`SIGINT`），确认进程立刻取消进行中的网络请求并干净退出，退出码为 `0`。

</details>
