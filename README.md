# mi-guard

<img src="https://raw.githubusercontent.com/lonelygo/mi-guard/main/deploy/macos/AppIcon.png" width="96" height="96" alt="mi-guard 通知助手图标">

`mihomo-guard` 是运行在 macOS 上的带宽选线守护进程。默认配置为 US 和 SG 两条 WARP 链维护前置节点。也可以让它直接管理普通的手动选择组，在当前线路持续降速或断线时为该组选线。手动 `bench` 可临时测试其他地区或新的机场订阅。

## 通过 Homebrew 安装

项目仓库同时作为 Homebrew tap。稳定版适用于运行 macOS 12 及以上版本的 Apple Silicon Mac。Homebrew 会校验发布包的 SHA-256 并安装预编译的 CLI 和通知助手。

```sh
brew tap lonelygo/mi-guard https://github.com/lonelygo/mi-guard.git
brew install lonelygo/mi-guard/mi-guard
mihomo-guard init    # 首次使用时创建配置，已有配置可直接沿用
```

Homebrew 把 `mihomo-guard` 链接到自己的 `bin`，同一版本的通知助手随包安装在 `$(brew --prefix mi-guard)/libexec/mi-guard.app`。默认的 `notifications.helper: auto` 会找到这个助手。已有 `make install` 安装的用户请参考下文的迁移说明。

也可以从发布页下载 Apple Silicon 的 `.tar.gz` 和 `SHA256SUMS`，在下载目录运行 `shasum -a 256 -c SHA256SUMS`。解包后保留目录结构，运行包内的 `bin/mihomo-guard` 即可找到配套助手。[下载发布包](https://github.com/lonelygo/mi-guard/releases/latest)

## 配置 guard 和接入 Mihomo

| 文件 | 用法 |
| --- | --- |
| `~/.config/mihomo-guard/mihomo-guard.yaml` | 你实际编辑的 guard 配置。首次用 `mihomo-guard init` 创建，已有文件会保留。Homebrew 登录服务读取此路径，命令行可用 `-c /path/to/guard.yaml` 指定其他文件。 |
| [share/mi-guard/mihomo-guard-default.yaml](share/mi-guard/mihomo-guard-default.yaml) | 当前版本的完整默认值、单位、字段说明和 HK/TW 配置示例。内容与该版本 `init` 生成的文件逐字节相同。升级后可用它对照自己的配置。 |
| [share/mi-guard/mihomo-overlay.yaml](share/mi-guard/mihomo-overlay.yaml) | Mihomo 的接线示例。将其中的条目手动并入生效的 Mihomo 配置，再重新加载。 |

从 v0.1.1 起，源码仓库和解压后的发布包都在顶层的 `share/mi-guard/` 提供这两份 YAML。`make install` 默认将它们复制到 `~/.local/share/mi-guard/`。Homebrew 用户可以用以下命令查看。

```sh
less "$(brew --prefix mi-guard)/share/mi-guard/mihomo-guard-default.yaml"
cat  "$(brew --prefix mi-guard)/share/mi-guard/mihomo-overlay.yaml"
```

在 guard 配置中，`mihomo.config_path` 指向正在使用的 Mihomo 主配置，`mihomo.stable_binary` 指向 `bench --source` 启动的独立内核。在线守护连接 `mihomo.controller` 指定的现有内核。文件路径支持 `~/`，相对路径以 guard 配置所在目录为基准。

在 **Mihomo 主配置**中，把接线示例的策略组和监听器分别加入现有的 `proxy-groups`、`listeners` 列表，并将 `use` 改为实际的 provider 名称。若主配置由订阅工具生成，请把这些条目维护在工具的持久覆写配置里。默认测速监听为 `127.0.0.1:17987`，测速组专供该监听器使用。第二个监听 `127.0.0.1:7898` 用于 US 的地区校验，仅已配置 `geo_proxy` 的组需要对应的出口监听。

```sh
mihomo-guard status
mihomo-guard check
```

`check` 核对生效的监听绑定、纳管组、链式前置引用和测速路径。`status` 显示当前组选择、账期用量及样本数，`status --probe` 还会检查实际链路或选择组，并显示已启用的地区校验结果。

### 增加地区或只管理一个手动选择组

在 guard 的现有 `groups:` 列表追加条目，名称使用 Mihomo 中的完整名称。下面的 HK 条目管理链式前置，TW 条目只切换指定的 `type: select` 组。候选节点需要同时属于该组和专用测速组。

```yaml
  - name: "🔞 WARP-HK"
    mode: chain
    chain: "⛓️‍💥 HK-WARP"
  - name: "🇹🇼 台湾节点"
    mode: selector
```

`groups` 按整个列表替换默认值。希望继续管理 US、SG 时保留原有两项。v0.1.0 配置省略 `mode` 时仍按 `chain` 处理。若今后只想管理原来的 `🇺🇲 美国节点`，可将整个 US 条目换成 `name: "🇺🇲 美国节点"` 和 `mode: selector`。新增的普通组可以先用 `mihomo-guard bench -g '🇹🇼 台湾节点'` 只看测速结果，再用同一命令加 `--apply` 应用选择。

## 手动测速

```sh
# 只测狮城候选，打印实际下载 Mbps、HTTP 探测延迟、失败次数及计费字节
mihomo-guard bench -g '🔞 WARP-SG'

# 临时测试任意已加载策略组；或按生效节点名、名字正则选择
mihomo-guard bench -g '日本节点'
mihomo-guard bench --node '控制器中显示的完整节点名'
mihomo-guard bench --filter '日本|Japan|JP'
# 临时切换为每节点四条下载流，总额度仍为 4 MiB
mihomo-guard bench --filter 'JP' --bytes 4194304 --streams 4

# 临时读取一个未加载到在线内核的 provider 文件或订阅 URL
mihomo-guard bench --source /path/to/provider.yaml -f 'JP|SG'
mihomo-guard bench --source 'https://example.org/subscription' --filter 'US'

# 主动把结果应用到一个已纳管的 WARP 前置组
mihomo-guard bench -g '🔞 WARP-US' --apply
```

无参数的 `bench` 测 `groups` 中的所有纳管组。在线模式从独立测速组可达的物理成员中选取候选，可以在接线示例的 `use` 列表加入其他已加载的 provider。每次只选一个测速节点，下载后恢复测速组原选择。每条下载连接在发出请求前，都通过运行中内核的连接表核对实际 selector 和节点，接线错误会中止本轮。默认只输出结果、保存历史及计费，显式 `--apply` 会执行业务组切换。手动指定地区、节点或正则时使用自己的实际名称。

`--source` 接受带顶层 `proxies` 列表的 Mihomo YAML，可重复使用。它用配置中的 `stable_binary` 启动临时核心，默认路径为 `~/Library/Application Support/com.metacubex.ClashX.meta/.private_core/com.metacubex.ClashX.ProxyConfigHelper.meta`。临时核心关闭 TUN，通过临时回环监听和独立 selector 串行测速，退出后回收进程和临时文件。独立模式会核对启动后的版本，默认要求正式版。需要试验 alpha 时可显式设置 `mihomo.allow_prerelease: true`。两个模式共享同一个 guard 配置的账期预算。若系统级 TUN 接管其他进程流量，使用独立实例测试前应确认该核心进程到机场服务器的连接直出，或采用在线测速组。

## 自动选线

| 设置 | 默认值 | 行为 |
| --- | --- | --- |
| 心跳 | 每 30 秒，3 秒超时 | 连续两次失败后轻量验证历史备选，恢复优先 |
| 全组下载 | 每 12 小时，每节点 4 MiB | 一次扫描两个前置组，直出组共用结果 |
| 在用节点快筛 | 每 2 小时，每节点 1 MiB | 可设 `active_interval: 0s` 关闭 |
| 降速准备 | `severe_drop_pct: 20` | 当前带宽 ≤ 正常样本中位数 × 0.8 后进入复测 |
| 复测 | 默认间隔 1 分钟，累计 2 次，单次最多 8 MiB | 确认降速且有更好备选才切 |
| 选优容差 | `near_best_pct: 20` | 本轮达到最快节点的 80% 即进入同一优先档 |
| 历史和预算 | 每节点 50 次，6 GiB/账期 | JSON 保存，每次实际读取的下载响应体字节计入账期 |

例如当前节点的正常水平为 8 MB/s，降到 6.4 MB/s 就开始准备。若当初选中的节点为当时最快速度的 80%，再下降 20% 相当于那次最快速度的 64%。这个乘积只用来理解两个阈值，实际判断分别使用当前节点的历史基准和本轮各候选的新测量。

带宽探测分别统计一次 204 和一次有界下载，响应体中断会记录失败。冷启动缺少历史时，可以在组上设置绝对下限 `min_mbps`。连续下载失败也会触发确认。连续两次链路硬故障可以越过性能冷静期。

同档候选比较应用层探测失败比例和历史低分位带宽，平手时优先留用原节点。历史记录含探测次数，HTTP 请求失败率用于描述线路可用性。每个周期 16 个节点各读满 4 MiB，每天两轮，30 天共 3.75 GiB 下载响应体。单节点默认单流，可用 `speedtest.streams_per_node` 配置 1～8 流，所有流平分节点总额度。协议开销和机场计费倍率另计。守护进程与手动命令共用配置和历史文件，互斥访问测速组、原子更新 JSON，进程异常退出时预留字节暂按已用处理。

切换时先调整 `🔞 WARP-US` / `🔞 WARP-SG`，再验证相应 WARP 链。US 可按预期 Google 地区顺位尝试。验证成功后，在候选仍属于同地区直出组的前提下，尽力同步 `🇺🇲 美国节点` / `🇸🇬 狮城节点`。直出跟随失败只影响直出组。守护进程会接纳手动选线，并在换线前重新读取当前选择。

## 登录时运行与系统通知

接线检查通过后，以当前登录用户执行以下命令。首次运行 `notify-setup` 时，在 macOS 的提示中允许通知。

```sh
mihomo-guard notify-setup
brew services start lonelygo/mi-guard/mi-guard
brew services info lonelygo/mi-guard/mi-guard
```

`brew services start` 启动守护进程并注册当前用户的登录项。日志写入 `~/Library/Logs/mihomo-guard.log`。全组扫描结束后发送一条摘要，切换通知会报告主组和直出组的最终结果。可分别关闭 `notifications.summary` 和 `notifications.switch`。横幅显示遵循 macOS 的通知设置与专注模式。

```sh
brew update
brew upgrade lonelygo/mi-guard/mi-guard                # 升级到最新稳定版
brew services restart lonelygo/mi-guard/mi-guard       # 升级或修改配置后重启
brew services stop lonelygo/mi-guard/mi-guard          # 停止并取消登录自启
```

### 从 `make install` 迁移

Homebrew 版沿用原来的配置和测速历史。启用 Brew 服务前，若旧 LaunchAgent 已加载，先执行 `launchctl bootout "gui/$(id -u)/com.lonelygo.mi-guard"`。若 `~/Library/LaunchAgents/com.lonelygo.mi-guard.plist` 存在，把它移出 LaunchAgents 目录，避免下次登录启动两份守护进程。`command -v mihomo-guard` 应指向 Homebrew 的 `bin`。

旧配置若指定 `helper: ~/Applications/mi-guard.app`，且旧 App 仍在，通知会继续使用它。将该行改为 `helper: auto` 后，通知会使用当前安装包中的图标和助手。自定义助手仍可使用自己的路径。

### 从工作副本构建

源码构建需要 Go 和 Xcode Command Line Tools。要通过 Homebrew 跟踪 `main`，可使用 `brew install --HEAD lonelygo/mi-guard/mi-guard`。在源码目录运行 `make macos` 可得到 `build/mihomo-guard` 和 `build/mi-guard.app`，可直接用 `./build/mihomo-guard` 运行上述 CLI 命令。已有手动部署也可继续使用用户目录安装。

```sh
make install
~/.local/bin/mihomo-guard notify-setup
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.lonelygo.mi-guard.plist"
```

## 验证与设计

`make test` 运行 Go 竞态测试和静态检查。`make smoke` 通过本机稳定内核、临时控制器和回环测试服务器检查 CLI 的手动测速和切换。可用 `make smoke STABLE_CORE=/path/to/mihomo` 选择测试用核心。[现行设计](https://github.com/lonelygo/mi-guard/blob/main/mihomo_guard_mac.md) · [实施计划](https://github.com/lonelygo/mi-guard/blob/main/.omx/plans/implementation.md)

维护者在 `main` 上推送带发布说明的 `vX.Y.Z` 标签后，GitHub Actions 会测试并打包、发布归档和校验文件、更新 tap，再从公开的 tap 安装并运行 `brew test`。[发布工作流](https://github.com/lonelygo/mi-guard/blob/main/.github/workflows/release.yml)

## 致谢与许可证

感谢 **faceair/clash-speedtest** 提供节点下载测速的实现，以及 **MetaCubeX/mihomo** 提供协议内核、策略组和外部控制器。下载器裁剪自 clash-speedtest 提交 `1e1fb9e2bbb29282cce5613a6c6b946b85169fdc`。修改包含本地监听器拨号、按节点限制读取量、取消处理、部分读取错误和实际耗时计量。mi-guard 采用 GNU GPL v3 发布。[上游测速源码](https://github.com/faceair/clash-speedtest/blob/1e1fb9e2bbb29282cce5613a6c6b946b85169fdc/speedtester/speedtester.go) · [Mihomo](https://github.com/MetaCubeX/mihomo/tree/Meta) · [许可证](LICENSE)
