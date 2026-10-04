# Project Replay 0.2.5

## 0.2.5 群组码连接与自动精选

- 三端统一升级：中继无需设备 Token / devices.json；客户端填写中继地址和相同四位群组码即可接入，设备 ID 自动生成。手动与自动配对仍可选。旧 systemd 启动参数请移除 `-credentials`。
- 新录制素材自动选入精选：半场包含设定半场结束回合以内的全部录制击杀，全场包含当前地图全部录制击杀（含加时）。比赛结束由 A 路 GSI `gameover` 自动触发全场精选，可关闭自动播出或手动排序、移除和播放。
- 精选等待尚在录制 / 回传的素材并追加播放；半场优先于回合，全场优先于其他播出。换地图或切换采集会话后清空列表。升级时将已有当前会话素材补入精选。
- 录制机默认 X 光开启，覆盖 CS2 启动、GOTV 连接、镜头切换、Demo 与录制；原生 / 自定义 HUD 均保持开启。


## 0.2.1 支持云服务器 IP 直连

三端支持仅填写云服务器 IP（默认 7790 端口），无需域名或证书；直接 IP 模式使用未加密 HTTP/WS，也可继续配置 HTTPS。新增独立云中继服务、四位数字群组、手动/自动配对、指令转发及校验后的视频分块回传。旧配置默认保持局域网直连，原节点 URL 不变。安装包和部署步骤见 [云中继部署与配对](docs/云中继部署与配对.md)。


CS2 双端回放软件首版：Windows 导播台 + Linux 录制 Agent。中文界面、单文件程序、无需安装运行时。界面由程序在本机提供，使用系统浏览器打开，关闭浏览器不会结束后台；在启动终端按 Ctrl+C 退出。

**这是可运行的 P0 首版，不是已经通过真实比赛验收的完整 V1。** 演示链路可独立运行；真实录制适配器已实现，实际 GOTV、视角模式、事件到视频定位及 Windows OBS 播出仍须现场联调。没有把演示素材当成真实录像。

## 下载安装

| 设备 | 文件 |
|---|---|
| Windows 10/11 x64 | `dist/ProjectReplay-windows-x64-portable.exe`，或附说明的 `ProjectReplay-0.2.5-windows-amd64-portable.zip` |
| DGX Spark / Linux ARM64 | `dist/ProjectReplay-0.2.5-linux-arm64.tar.gz` |
| 普通 Linux x64 | `dist/ProjectReplay-0.2.5-linux-amd64.tar.gz` |

Windows：将 EXE 放入可写目录，双击启动，浏览器自动打开本地导播台。免安装、不需要 Python/.NET/Node；EXE 未做代码签名。首次运行在 EXE 同目录创建 `replay-data`，包括配置、素材和状态；移动程序时一并移动此目录。ZIP 内含启动脚本、说明和许可证。浏览器未打开时，使用终端显示的地址。

Linux 本地体验：解压后运行 `./project-replay`。无桌面的节点运行：

```bash
./project-replay -role agent -listen 0.0.0.0:7788 -no-browser
# 或 ./start-agent.sh
```

在节点本机打开 `http://127.0.0.1:7788`，或 Windows 访问 `http://节点IP:7788`，直接进入，无需登录或配对。默认只监听本机；启动脚本为局域网监听。HTTP 适用于可信局域网，跨公网必须在外部配置 HTTPS/VPN，勿直接暴露 OBS/NetCon。

## 三分钟体验

1. 双击 Windows EXE，或 Linux 执行 `./project-replay`。
2. 默认演示模式，点击「演示一轮」。
3. 同一时段一名选手单杀，另一名选手双杀。调度器应选择双杀，共用一次采集，约 12 秒后出现两段1.5 秒素材。
4. 点击素材预览；点击「加入待播」并调整顺序。
5. 演示使用内置彩色测试图和测试音频，不需要游戏、OBS、FFmpeg或网络。演示模式可通过网页输出测试播出，不控制 OBS 场景。

## 本地 Demo 双路联调

两端载入本地 Demo 时都会先执行 `cl_demo_predict 0`，再执行 `playdemo`；Linux 无头启动也自动带上 `+cl_demo_predict 0`。

新增两端本地 `.dem` 测试入口：分别载入同一文件、定位到同一 tick 并检查就绪，在 Windows 设置 Linux B 路延迟后统一启动。沿用真实 GSI、OBS 录制、回传及网页输出链路。具体步骤与验证边界见 [本地 Demo 双路联调](docs/本地Demo双路联调.md)。

## 实时双端配置

### Linux Agent

解压后运行 `./start-agent.sh`，自动打开 Linux 中文图形界面（系统浏览器）。无桌面服务器使用 `./start-agent-headless.sh`；也可使用 `./start-agent.sh -no-browser`。

Linux 界面的“本地 Demo 双路联调”区提供“无头启动 CS2 并开启监听”：复用已安装的 `cs2-headless-steam.service`（显示器 `:20`）及 Snap Steam 命名管道，自动附加当前本机 NetCon 端口、`-insecure` 和 1920×1080 参数。启动状态会持续显示，最长等待 90 秒，只有游戏进程在 `:20` 且 NetCon 命令回读成功后才报告就绪。需要已有无头 Xorg、窗口管理器、音频及已登录的 Steam；不会自动安装这些服务，也不会重启 OBS。游戏已在其他显示器运行时会拒绝启动；游戏已在 `:20` 运行时只检查响应，不重复启动。桌面 Steam 已运行时普通启动按钮会提示冲突，不再向桌面 Steam 发送启动命令。新增“切换 Steam 到无头并启动（退出桌面 Steam）”按钮：退出当前 Steam 客户端及辅助进程，再启动专用 `:20` Steam；若检测到仍在运行的游戏则拒绝切换。切换不会同时保留桌面 Steam 客户端，也不操作 OBS。

Linux 界面的“本地 Demo 双路联调”区新增“一键彻底关闭 CS2”：无需 NetCon 响应，强制结束当前用户的 CS2 游戏进程，取消本机 Demo 倒计时并暂停采集；不关闭 Steam、OBS，不删除录像。正在录制 / 处理任务或会话被其他录制程序占用时会拒绝操作。

界面顶部的原生 HUD「显示 / 隐藏」现在读取 NetCon 回执和实际变量，恢复雷达、击杀提示和队友标记的强制隐藏设置。游戏拒绝命令或未回读时明确报错，不再把 TCP 写入成功当成 HUD 启用成功。`cl_drawhud` 是带 cheat 标志的游戏变量，服务端权限仍可能阻止切换。

新增独立战队 HUD：在 Windows 填写初始 CT / T 战队名称，上传 PNG / JPEG 图标（各 ≤256 KB、≤2048×2048），点击「保存并同步 Linux」。Linux 点击「安装战队 HUD 到 OBS」，会在当前节目场景创建 / 更新浏览器源并置顶，按画布大小缩放；地址为本机 `/hud.html`。该 HUD 使用 B 路 GSI 显示战队名称、图标、比分和时钟，默认常规半场 12 回合、加时半场 3 回合，可按赛制调整。没有 GSI 时保持透明。必须安装到实际录制使用的场景；战队 HUD 安装时会自动发送 CSStudio 的关闭游戏 UI 指令并回读确认，成功后录制和切镜头沿用该模式。当前战队 HUD 仅包含战队信息、比分和时钟。

Windows 新增「一键清理两端 Replay 临时文件」：先清理 Linux，成功后清理 Windows。保留连接、采集、播出设置、战队图标及当前转场；删除素材、未完成下载、旧转场、事件、队列和本版本登记的 OBS 原始录制。节点离线或仍有任务时不清理本机；失败可重试。不会扫描删除整个 OBS 视频目录，不删除用户 Demo、CS2、Steam 或外部程序文件。升级前未登记的外部 OBS 原始录像无法可靠区分归属，需自行核验。

点击「两端启用紧凑录制间隔」可更新旧配置：设备准备 0.2 秒、额外切换间隔 0 秒，保护量为 `max(0.2, 配置误差 + 0.05)` 秒。通常不同视角的完整1.5 秒片段要求击杀间隔至少约 2.1 秒；同视角的重叠击杀复用仍在进行的录制，各自生成1.5 秒片段。硬件启动/停止耗时和实际视角重叠仍会限制可录制数量。详情见 [0.1.1 修复与升级说明](docs/0.1.1修复与升级说明.md)。

沿用现有 Steam/FEX、Xorg、CS2 和原生 OBS 环境；软件不自动启动、重启或清理这些现有服务。

- 本机运行 CS2 观战客户端与 OBS，并提供 NetCon（例如 `127.0.0.1:2121`）、OBS WebSocket 5.x（已有部署可用 `ws://127.0.0.1:4466`）。填入实际密码。
- 安装/配置本机 FFmpeg、ffprobe；FFmpeg 必须含 libx264。软件调用外部二进制，不随包分发。
- 配置 CSStudio 的实际 `session.lock` 绝对路径。Replay 同时争用该锁及 `/tmp/cs2-recorder-UID.lock`，拒绝与现有批录制并行使用会话；以同一 Linux 用户运行。锁目录必须已经存在。
- 使用真实模式；设置比赛和地图；epoch 是各端内部版本，无需对齐。槽位映射可留空：程序先按 B 路唯一玩家名切人并核验 SteamID。同名或特殊名字可填已确认槽位，如 `{"76561198000000001":14}`。不会把 observer_slot 当作命令槽位。
- Linux Agent 启动时自动检测普通 Steam、Snap、Flatpak 及 Steam 自定义库，在 CS2 的 `game/csgo/cfg` 写入或更新 `gamestate_integration_replay_b.cfg`，使用当前监听端口，不改动其他 GSI 文件。找不到目录、存在多个安装或写入失败时，界面日志会提示原因，程序仍正常启动。可用 `./start-agent.sh -cs2-cfg "/绝对路径/game/csgo/cfg"` 指定目录。若游戏已在运行，请重启 CS2 加载配置；不会自动重启游戏。仍可在界面手动下载配置。
- 支持直接粘贴平台指令 `connect IP:端口;password xxx`，也兼容 `host:port`。在 Windows 保存配置并勾选自动发送，或直接点击「连接 Linux GOTV-B」，即可转发 Linux。程序解析地址和密码后，先执行 `password "xxx"` 再执行 `connect IP:端口`；含空格的密码须用双引号包裹。密码独立保存，不通过状态接口或日志回显；同一地址留空保留密码，换新地址且未提供密码时清除旧密码。可用 `password ""` 显式清除密码。连接命令使 Linux epoch 增加并重置时间线，GSI 自动录制无需手工校准。
- 默认开启“自动录制 / 发送任务（近似定位）”：GSI 识别击杀后自动规划，满足时序、映射和节点条件时自动提交；普通 GSI 击杀按回合号与阶段倒计时自动发送，无需校准或 tick。素材会标注 `estimated`。严格模式可在设置中主动开启，但目前会阻止真实出片。升级旧版时首次迁移为上述自动近似采集默认值。手动暂停会关闭自动录制，后续校准不会擅自恢复；重启保留自动采集开关；换图或回档后等待新鲜 GSI，旧任务失效。
- 在 A/B 客户端观察同一时刻，录入 B 落后 A 的实测秒数和误差，点击「应用实测校准」。校准十分钟过期；断流、回档、换图会使其失效。启用采集。

### Windows 导播台

- 配置真实模式和相同比赛名称；地图由 Windows A 路 / Linux B 路 GSI 各自自动识别，不再手动填写。换图自动切换时间线并清空旧回放队列；节点 epoch 自动识别。保存仍会更新本地版本，使旧任务失效。
- 填入 Linux 节点 URL 。本机 OBS 参数应指向 Windows OBS，不是 Linux 的录制 OBS。
- Windows 导播启动时自动查找 Steam 安装及自定义库，安装 `gamestate_integration_replay_a.cfg` 到 CS2 的 `game/csgo/cfg`，使用当前端口；若游戏已经运行，需重启 CS2。找不到或发现多个安装时可用 `-cs2-cfg` 指定目录，也可继续手动下载 A 路配置。
- GSI 自动击杀读取 `allplayers.state.round_kills` 差分；字段缺失时兼容同回合 `allplayers.match_stats.kills` 差分。缺少基线和字段缺失不虚构击杀；跨回合按重置后的 round_kills 或累计击杀增量识别首杀，一次增量大于 1 保留候选组。界面“GSI 自动击杀”显示接收状态、计数字段数、识别次数、采集阻断原因。事件来源需为 `gsi`；`parser` 模式只接收解析器事件。
- 普通 GSI 击杀无需填写人工时间差，也不用点击同步。以下校准仅用于旧版绝对时间 / 解析器事件路径。时钟同步通过五次 HTTP 往返取最低 RTT 样本，**不等于游戏时间线校准**。连接同步有效期 60 秒，配置节点后后台每 20 秒刷新；每次任务提交前自动刷新，无需手动同步 epoch。
- 关闭严格模式以进行近似链路联调，然后启用采集。可从「＋」手动加入指定选手、B 路未来时刻的候选。
- GSI 击杀在 Windows 检测后自动发送，Linux 按 B 路回合时钟规划并排队；已提交任务不抢占。控制请求结果不确定时查询同一 job_id，失败暂停并要求在节点核对。

### Windows OBS 网页输出

1. 在导播台「OBS 网页输出」复制地址，粘贴到 OBS **浏览器源**，尺寸设为节目分辨率（例如 1920×1080），放在直播画面上方。关闭「不可见时关闭源」和「场景激活时刷新浏览器」。输出地址为 `/output.html`，无需密钥或登录。
2. 空闲时网页完全透明；播放顺序为 **转场 1 → 回放列表 → 转场 2 → 透明**。在软件里分别导入两段 MP4 / WebM 转场，带透明通道的转场请使用带 Alpha 的 WebM。单文件最大 256 MB，单项播放超时为 120 秒。没有导入的转场会跳过；不支持的编码会报告播放失败并恢复透明。
3. 新素材自动进入当前会话的回合列表。选择回合可上移、下移或移除条目；移除只影响列表，不删除原视频。回合播出严格筛选对应回合，已开始的片段保持顺序，晚到素材追加到末尾。
4. 勾选「回合结束自动回放」后，由 A 路 GSI 的回合结束变化触发。默认等待 3 秒（可设置 0–60 秒），先播放就绪素材，未录制或仍在回传的素材就绪后追加。已手动播出的回合不再自动触发。下一回合倒计时到 1:47、产生首杀或导播手动返回直播时恢复透明，晚到素材不重新触发已结束会话。
5. Windows 全局快捷键默认 **Ctrl+Alt+R**，软件内可以修改或清空关闭，支持 Ctrl / Alt / Shift 加字母、数字或 F1–F11。界面会报告注册成功或冲突。软件后台运行时生效；触发当前回合，在结束阶段触发刚结束的回合。也可点击「播出此回合」，或选择素材后点击「播出本回合」。
6. 新素材自动加入半场 / 全场精选；半场仅收录设定半场回合以内的素材，全场包含当前地图全部录制击杀（含加时）。每条回合素材旁的 ☆ 或预览区「选入半场回放」仍可补选。精选列表独立排序、移除；不会因为从普通回合列表移除而取消精选。默认第 12 回合结束触发半场，可改为其他回合；也识别 GSI `halftime` / `intermission` 的进入变化。半场优先于普通回合播出；A 路进入 `gameover` 时全场精选优先播出，重复快照不重复触发。尚有录制 / 回传时等待素材并追加；没有素材且没有待完成任务时不播出。
7. 回放视频右上方使用 **REPLAY / BETA 动态标签**：转场 1 后随首段回放滑入，多段片段之间保持，末段结束前约 0.4 秒滑出；转场与空闲时不显示。可随时按界面「返回直播」或导播页面 Esc 恢复透明。输出断开或视频解码失败也会中止播出；日志和输出状态可查原因。
8. 请重新下载 A 路 GSI 配置，本版增加了 `round` 与 `phase_countdowns` 订阅。全局快捷键无需导播页面保持前台；设置在独立「保存播出设置」按钮保存，不改变采集 epoch。更换比赛/地图或保存会话配置后，旧队列会清空，旧素材保留在素材库。

演示模式也可显式触发网页播放以验证链路，但不会控制 OBS 场景或游戏。浏览器源一次只允许一个页面作为播放端；关闭普通浏览器内的输出测试页，再在 OBS 中使用。OBS 浏览器源音频由 OBS 配置路由，软件不会自动静音直播场景中的游戏或解说音频。常规浏览器可能阻止带声音的自动播放，正式播出请使用 OBS 浏览器源。

Windows 二进制已交叉构建；全局热键注册、Alpha 转场、OBS 音频及真实比赛 GSI 时序仍需在 Windows 上实测。Linux 测试覆盖服务端状态机、模拟 GSI、列表持久化、转场导入、免配对输出、断线及媒体失败恢复。

## 已实现与边界

| 能力 | 本版状态 |
|---|---|
| 中文 UI、视频预览、搜索、回合列表、半场 / 全场精选自动收录、排序及移除、日志 | 已实现，浏览器本地界面 |
| OBS 透明网页输出、双转场、Replay 标识、回合 / 半场 / 全场自动播出 | 已实现，真实比赛与 Windows OBS 待实测 |
| 软件内配置 Windows 全局热键 | 已实现 RegisterHotKey，已交叉编译，待 Windows 实测 |
| 免配对双端连接、任务幂等、状态查询、断点下载和 hash 校验 | 已实现 |
| 时间窗口 DAG 选择、同人合并采集、手动指定、已提交保护 | 已实现，确定性测试含穷举对照 |
| A/B GSI 接收、推断击杀、身份监控 | 已实现；实际 CS2 字段须联调 |
| NetCon 切人、OBS Start/Stop、1.5 秒 H.264/AAC 裁剪 | 已实现适配器；未运行真实比赛 |
| 数据持久化、重启保留自动采集开关、目录进程锁 | 原子 JSON 快照，首版未使用 SQLite |
| 控制协议 | HTTP JSON + 状态轮询，首版未使用持久 WebSocket；OBS 使用 WebSocket |
| 精确 Demo/CSTV+ 事件解析、自动游戏时间线对齐 | 未实现；严格模式阻止伪精确出片 |
| 双 OBS 媒体源预装、定位校正、自动清理/归档、云节点 | 未实现 |
| Windows 原生窗口、托盘 | 未实现；便携 EXE 在浏览器打开本地界面，保留启动终端 |

单 Worker 同时执行一个提交批次，在素材处理/回传完成前不会启动下一批；因此实际吞吐低于理想调度模型，密集不同视角任务可能错过。当前不支持在已提交采集中追加后续新击杀。依赖 GSI 的身份监控只能检查采样点，不证明两次采样之间的所有帧都正确，也不能独立确认第一人称模式。OBS 录制时间推算属于近似时间，不宣称击杀精确居中。

首版状态最多保留 5000 个事件，素材无自动删除。长比赛前检查磁盘，退出后备份 `replay-data`；需要新归档时使用新的 `-data` 目录。失败留下的原始 OBS 录像由 OBS 的原配置管理。应用不会自动删除用户录像。

## 开发和复现构建

Go 1.25+；本次使用 Go 1.27.1，唯一模块依赖固定为 gorilla/websocket v1.5.3。交叉构建不使用 CGO、Wine 或 Windows SDK。

```bash
go mod download
go test -race ./...
REPLAY_MEDIA_TEST=1 go test ./cmd/... ./internal/...   # 附加 ffprobe 演示媒体检查
node tests/output.test.cjs         # 网页播放状态机（开发验证，不是运行依赖）
python3 scripts/build.py
```

构建脚本接受 `GO` 环境变量指定编译器路径。输出 Windows x64 便携 EXE/ZIP、Linux ARM64/x64 tar.gz，以及 `dist/SHA256SUMS`。构建关闭 VCS 自动戳，以支持没有可读 Git 元数据的源码目录。

## API 摘要

所有 `/api/*` 请求直接访问，不使用 token、登录或配对。POST 为 JSON，文件下载支持 Range。监听地址范围内的主机均可调用接口。

- `GET /api/state`、`GET /api/time`
- `POST /api/config`、`/api/calibrate`、`/api/pause`、`/api/remote-sync`
- `POST /api/events`（使用稳定 id 更新未提交事件）、`/api/pin`
- `POST /api/jobs`、`GET /api/jobs/{id}`
- `GET /api/media/{artifact_id}`
- `POST /api/output/settings`、`/api/output/queue`、`/api/output/play`、`/api/output/stop`
- `POST /api/output/transition/{1|2}?name=effect.webm`（原始文件上传，不使用 JSON）
- `/output.html` 与 `/output-api/*` 无需密钥，输出媒体接口仅返回当前播放列表的媒体
- 兼容旧版：`POST /api/queue`、`/api/play`、`/api/return-live`（旧 OBS 场景 API；新版 UI 使用网页输出）
- `POST /api/round-events`、`GET /api/round-events/{id}`：回合时间击杀提交和状态查询
- `POST /gsi/a`、`/gsi/b`（无需 auth.token；兼容旧配置中多余的 auth 字段）
- `GET /api/gsi-config/a`、`/api/gsi-config/b`

任务时间单位为目标 Agent 的 Unix 毫秒；导播在提交前应用实测机器时钟偏移。协议目前为首版内部 API，调用方使用固定版本，不应把它视作正式稳定的 Valve 接口。

### 道具击杀镜头

已加入按具体道具实例调度、道具镜头适配器客户端、飞行到作用区域的反馈校验，以及手动事件录入和素材证据。已知道具击杀不会静默退回投掷者视角。现在可用 replay-source 自动解析事件并通过 native 后端控制 CS2 镜头；现有 GSI 差分本身仍不能识别道具击杀。接入协议与限制见 [道具击杀追踪](docs/道具击杀追踪.md)。

## 自动追雷（原生后端）

已包含 `replay-source` 解析器和 Linux 原生 CS2 镜头跟随。真实 Demo + OBS 的手雷、火区1.5 秒片段已验证；在线 GOTV 仍需实际数据源与时间校准。

配置 `event_source=parser`、`tracking_mode=native`；使用录制机的 NetCon 和 OBS。完整启动命令、样片与限制见 [原生自动追雷交付与验证](docs/原生自动追雷交付与验证.md)。

最新调度、无预热录制和 Windows Workshop Tools 后台渲染说明见 [自动录制流程与后台渲染](docs/自动录制流程与后台渲染.md)。

### 自动隐藏游戏 UI 与最低画质

Linux 安装战队 HUD 到 OBS 时，真实模式会按 CSStudio 的 `openhud_headless.json` 发送 `sv_cheats 1; gameui_hide; hideconsole; cl_drawhud 0; crosshair 0; demo_ui_mode 0; cl_drawhud_force_radar -1; cl_drawhud_force_teamid_overhead -1; spec_show_xray 1`，并回读确认。游戏拒绝时会报告失败。成功后保存战队 HUD 模式，录制、切换镜头、Demo 播放及重新启动游戏时沿用；点击原生 HUD 按钮可退出该模式。

通过 Replay 启动新的 CS2 进程前，自动备份并原子更新 Snap Steam 账户的 `cs2_video.txt`：关闭抗锯齿、环境光遮蔽和垂直同步，阴影、纹理、粒子、过滤采用最低设置，HDR / FSR 采用性能设置，输出保留 1920×1080。配置缺失、字段异常或发现多个账户时会报错并停止启动。已经运行的 CS2 不会被强制重启，画质在下次关闭后重新启动时应用。

回放窗口现为击杀前 1 秒、击杀后 0.5 秒，总长 1.5 秒；采集保护量不计入成片。GOTV 连接、CS2 启动和录制默认发送 `spec_show_xray 1`，自定义 HUD 保持 X 光开启。命令说明见 [CS2 ConVar 转储](https://cs2.poggu.me/dumped-data/convar-list/)。

## 项目许可证

Project Replay 自 0.2.1 起采用 [Apache License 2.0](LICENSE)。Copyright (c) 2026 Project Replay contributors。第三方依赖继续遵循其各自许可证，详见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) 和 `third_party_licenses/`。

### Linux 网页一键启动无头 OBS（0.2.2）

Linux 录制台的「一键启动并配置 OBS（无头）」使用本机已有的
`cs2-headless-obs.service`，保持 `DISPLAY=:20`，与无头 CS2 共用显示器。
要求该用户服务使用 `~/.config/cs2-headless` 独立 OBS 配置目录；缺少无头会话时网页会明确提示，不会改用桌面显示器。

按钮会启用专用 OBS 的 WebSocket（保留已有端口和密码；新配置默认 4466 并生成密码），
启动服务及其显示、音频依赖，验证连接后自动保存 Replay 的 OBS 地址和密码。
首次修改会备份原 WebSocket 配置，运行中的 OBS 不重启、不改写配置。
启动完成后可点击「安装战队 HUD 到 OBS」。录制或 Demo 运行期间禁止启动配置。
启动状态显示在按钮旁；失败时可查看 `journalctl --user -u cs2-headless-obs.service`。

### 自定义 / 独立程序 HUD（0.2.3）

Linux 录制控制 →「自定义 HUD 设置」支持：

- **内置战队 HUD**：恢复 Replay 的战队叠加层。
- **独立 HUD 程序的网页输出**：输入 OpenHUD 等程序提供的透明网页输出地址及尺寸；Replay 在当前无头 OBS 场景创建浏览器源。比如本机 OpenHUD 录制模式可提供 `http://127.0.0.1:1350/recorder`，请以实际启动端口、输出路由为准。
- **已有 OBS 源**：读取无头 OBS 的源列表，选择独立 HUD 对应的浏览器源或窗口采集源。Replay 保留该源的地址、尺寸、裁剪及滤镜，只将其加入当前场景、启用并置顶。

独立 HUD 程序需先运行，并使用它自身的配置接收 CS2 GSI；Replay 不会自动适配第三方协议、启动任意可执行文件或重写其比赛名单。网页输出由 Linux OBS 访问；Windows 浏览器中的 localhost 不是 Linux 服务地址。只有窗口输出的程序需运行在 `DISPLAY=:20`，并提前在该无头 OBS 添加窗口采集源。

「保存并应用到 OBS」持久保存所选 HUD；切换时隐藏前一个 HUD 源，保留源本身。
可勾选「保留游戏原生 HUD」。默认隐藏游戏原生 HUD，并在后续 CS2 启动和录制切镜头时保持。
OBS 正在录制或直播时禁止切换。游戏尚未启动时，OBS 安装成功并显示游戏状态未同步提示；启动游戏后可再次点击「应用 HUD 到 OBS」。

### ZIP HUD 导入并自动启用（0.2.4）

Linux 录制控制 → 选择本地 `.zip` →「导入并自动启用」。上传完成后自动安全解压，识别 HTML 入口，启动/连接 `DISPLAY=:20` 的 OBS，将独立浏览器源加入当前场景并置顶，隐藏之前选用的 HUD。无需手动配置 URL 或启动 HUD 程序。保留原包资源；每次导入独立目录和 OBS 源，失败时尝试恢复原场景的可见状态。

支持根目录 `index.html`，或 ZIP 中唯一的 `index.html`（可带顶层文件夹）。多个入口时添加 `replay-hud.json`：

```json
{"name":"我的 HUD","entry":"index.html","width":1920,"height":1080}
```

`entry` 相对于 manifest 所在目录，资源使用相对路径；宽高默认 1920×1080。
上传限制为 100 MB，解压总计 512 MB、单文件 64 MB、HTML 入口 8 MB、最多 4000 项；拒绝目录穿越、符号链接和重复路径。

这是网页 HUD 静态包加载器，不执行 ZIP 中的安装脚本或服务端程序。React/Vue 项目应上传构建产物。依赖 OpenHUD 等专用 Socket.IO/API 服务的包需要适配该协议，不能仅凭 ZIP 自动推断所有第三方逻辑。

Replay 自动注入只读数据桥：

- `window.ReplayHUD.state`：最近一次数据，包含 `gsi`、`teams`、`fresh`、`visible`。
- `window.addEventListener('replay-hud', e => ...)`：`e.detail` 为上述数据，每约 100 ms 更新。
- `window.addEventListener('gsi', e => ...)`：`e.detail` 为原始 CS2 GSI，剔除认证信息。
- `fetch('/hud-api/state')`：内置 HUD 的战队名称、图标、比分、回合与倒计时接口。

导入 HTML 使用 sandbox 隔离，仅开放 HUD 只读数据接口；无法借导入脚本调用 Replay 控制 API。游戏未启动时先启用 OBS 图层，后续启动 CS2 时应用隐藏原生 HUD 的设置。选择「自定义 HUD 设置 → 已导入的 ZIP HUD」可重新启用上次导入的包。
