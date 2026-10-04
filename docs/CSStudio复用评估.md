# CSStudio 本机复用评估

核查日期：2026-09-16。参考项目：`/home/water/Project Autocs2video`，入口 `cs2-studio`。

本次范围：只读检查项目说明、关键源码、任务报告、素材存在性，并用 ffprobe 抽检一个素材。未修改 CSStudio、未启动游戏/OBS、未重启服务、未重新跑录制测试。本次只更新 Project Replay 规划文档。

## 1. 对前一版规划的修正

- **Spark 运行录制链路已有依据。** 不再将 ARM64 基础兼容性作为尚未解决的首要风险，沿用 CSStudio 的 Steam/FEX、专用 Xorg、原生 OBS 和 x264 配置。
- **两路延迟可自定义。** 用户已确认；后续只需明确配置入口、软件能否调用及实际生效过程。
- **Linux 执行端优先复用 Python 代码。** 取消从零用 .NET 重写录制底层的建议；Windows GUI 可以独立选择 WPF，双方通过版本化 JSON 通信。
- **先复用 OBS Start/Stop。** 回放缓冲是优化候选，不能把尚未接入的接口当作已有成果。
- **核心新增工作是实时适配。** CSStudio 已解决的是离线 Demo 解析与录制，实时 GOTV 的进度、事件采集和调度仍需实现。

## 2. 可核查的本机证据

| 证据 | 本次核查结果 |
|---|---|
| [HEADLESS.md](</home/water/Project Autocs2video/HEADLESS.md>) | 记载 Steam/FEX、NVIDIA Xorg `:20`、OBS XSHM、独立音频及 GSI 身份校验的部署过程 |
| [正式录制报告](</home/water/Project Autocs2video/jobs/desktop/双日凌空对复旦校队-无字幕/recording/match_f4347bfaf04894b5/real/capture_report.json>) | `backend=real`，`status=success`，92 个 capture 均记录 success、camera_verified=true |
| 上述报告引用文件 | 本次逐一检查 92 个文件均存在且非空；未重新解码全部文件或重算全部 hash |
| [第一段素材](</home/water/Project Autocs2video/jobs/desktop/双日凌空对复旦校队-无字幕/recording/match_f4347bfaf04894b5/real/captures/round_001_capture_0001.mkv>) | 本次 ffprobe 成功：H.264、1920×1080、60/1 fps、AAC、12.384 秒、22,352,617 字节 |
| 第一段历史质量记录 | 报告记载 743 帧，变化比例约 99.87%，黑屏 0 秒，音频峰值 -2.9 dB；这是既有报告结果，本次未重跑内容质量检测 |
| 第一段镜头证据 | `method=gsi_steamid`、`slot=14`；报告明确仅验证开录前身份，非全程跟踪 |

证据优先级：当前代码与现存素材/报告 > 有日期的部署记录 > README 中旧阶段说明。README 同时保留早期“未通过完整录制”和后来“已通过无头录制”的叙述，不能只取旧段落否定后来的成果。

`HEADLESS.md` 提到的 `jobs/headless-validation/validation_summary.json` 当前未找到，因此没有将它当作本次已读取的证据；使用仍存在的正式任务报告及素材补充核查。

上述证据足以支持“已有可复用的本机离线录制链路”，不足以证明当前实时 GOTV 接入、Windows 回传或全场视角监控已经完成。

## 3. 可复用模块与适配边界

| 模块 | 已有能力 | Replay 的处理方式 |
|---|---|---|
| `recorder/headless.py` | 专用显示、音频、OBS、Steam 会话准备、后台执行及会话锁 | 复用准备逻辑，新增持续在线会话生命周期 |
| `recorder/vconsole.py` | NetCon 通信、响应标记、超时、控制台反馈 | 复用通信；直播进度不能直接使用 Demo 查询函数 |
| `recorder/backends.py` | 切 POV、GSI 身份确认、OBS Start/Stop、状态与所有权 | 提取或包装通用操作；隔离 load/seek/pause/quit 等离线行为 |
| `recorder/gsi.py` | 本机 token 校验、CS2 appid 校验、当前观察者 SteamID 等待 | 扩展实时状态接收器；保留已验证的身份确认用途 |
| `recorder/obs_capture.py` | 已有捕获源刷新、XSHM 等适配 | 沿用当前源；在线切人不应每次执行重型环境初始化 |
| `recorder/verifier.py`、`media_quality.py` | 文件/视频检查、动态画面、黑屏和音频检测 | 复用出片校验；实时队列中测量校验耗时，必要时分级进行 |
| `recorder/worker.py` | 显式状态、报告、重试、素材复用 | 复用通用组件和报告思路，新增 LiveWorker，保留原 Demo Worker |
| `parser/demo_parser.py` | demoparser2 整文件解析 `player_death` 和 tick | 复用事实结构与离线核验；不能认定已支持增量流 |
| `desktop_app/` | 项目、素材、预览和后台状态管理 | 参考流程和数据模型；Linux GTK 界面不是 Windows 原生界面现成实现 |
| `editor/` 和击杀对齐文档 | tick—素材秒数映射、裁剪、hash关联 | 复用原则和可用逻辑；在线数据需提供新的时序样本 |

`recorder/broadcast.py` 管理的是队伍、名单、队标等展示元数据，不是 GOTV/CSTV 网络接收器。不能因为文件名包含 broadcast 就认定已有直播支持。

## 4. 两个重要的接口差异

### 4.1 GSI 代号与切人命令参数不同

当前 [models.py](</home/water/Project Autocs2video/recorder/models.py>) 中 `spectator_slot(user_id)` 返回 `user_id + 1`，并允许 user_id 为 0～63；该规则用于现有 Demo 任务。实际成功样本的命令槽位为 14。

因此协议必须分别保存：

```text
steamid64          稳定选手身份
gsi_observer_slot  观察者显示代号或快捷选择槽位
engine_user_id     当前游戏会话实体身份信息
spec_player_slot   NetCon 切人命令参数
mapping_epoch      对应哪次连接/地图/时间线
mapping_evidence   映射如何被验证
```

不能把 GUI 中的 1～0 玩家代号直接插入 `spec_player`；也不能未经验证将 Demo 的换算规则推广到所有直播实例。切换后以 B 路新鲜 GSI SteamID 再确认。实时模式增设观察对象持续监测，避免死亡后自动切人仍被报告成全程正确视角。

### 4.2 Demo tick 查询不等于直播进度接口

[Netcon.info()](</home/water/Project Autocs2video/recorder/vconsole.py>) 实际发送无参数 `demo_gototick`，解析 `Currently playing ... ticks`。部分旧文字说明写作 `demo_info`，以当前代码为准。

现有 Worker 依赖：`playdemo` → `demo_pause` → `demo_gototick` → 镜头配置 → StartRecord → `demo_resume` → 等待 tick → `demo_pause` → StopRecord。

实时 Worker 应改为：

```text
持续连接 GOTV-B
→ 持续接收 B 路状态与进度锚点
→ 接收已选定的录制任务
→ 校验剩余提前量及资源冲突
→ 提前切人、确认身份
→ 开始录制
→ 根据已验证时间映射结束录制
→ 检查素材、回传
→ 保持在线等待后续任务
```

直播模式不能调用 Demo seek、假设可以暂停网络比赛、对过期窗口离线式重试。重新连接后先校准再接受新任务；错过的事件只在有可回放数据源时安排补录。

## 5. 对新协议与部署的建议

保留 `cs2-record-job/1.0～1.2` 原有 Demo 语义，新建 `cs2-live-session/1.0` 与 `cs2-live-capture-job/1.0`，不要在旧任务中填入假的 demo_path 或 seek_tick。报告保留 camera evidence、实际边界、时间不确定性、媒体 hash、失败原因。

录制任务的选手和优先级由 Windows 导播/调度侧确定；Linux 执行节点只做确定性检查、按任务执行、反馈能力和冲突，不引入 LLM 自行决定录谁。

CSStudio 和 Replay 使用同一套 CS2/OBS 时，必须争用同一个有效会话锁或统一会话管理入口。不能各自创建不同锁文件后误认为已经互斥。已有 `HeadlessSession.run()` 会在 finally 中清理会话并停止有关服务，实时路径不能直接调用整套批任务入口。

沿用专用 OBS 4466、NetCon 2121、GSI 27654 的现有配置思路；新 Agent 单独提供局域网免配对接口，不把 NetCon/OBS 控制端口直接暴露给 Windows。GSI 端口不能被两个接收进程重复占用，应由会话所有者统一接收并分发。

既有代码标注 GPL-3.0-or-later，复用时保留来源和许可。具体打包边界在实现阶段确定，本次不复制或改写原项目源码。

## 6. 修订后的首版开发顺序

1. **实时接入实验**：在可用的独占录制时段，复用现有环境连接 GOTV-B，验证 NetCon 切人、身份反馈和持续录制；保留原 CSStudio 离线工作流。
2. **事件及时序实验**：Windows A 路 GSI + 精确事件候选适配器；核验是否能实时获得精确击杀 tick，同时为 B 路建立可观测的播放时间映射。
3. **可调延迟实验**：用实际配置入口调整 A/B 时间差，测试稳定阶段和改值过程；按前置长度、识别耗时、切人耗时和误差计算允许值。
4. **手动完整链路**：Windows 下发已知选手任务 → Spark 录制 → 回传 → Windows OBS 播放。
5. **自动任务与 GUI**：事件候选、来源标注、冲突处理、素材库、预览、待播列表和返回直播。
6. **持续运行验收**：在线掉线重连、回档/换图、录制中身份变化、回传中断和与 CSStudio 的资源互斥。

剩余最大的技术不确定性是“无服务器插件条件下的实时精确事件与播放进度”，而非 Spark 是否有可工作的录制环境。完整规划已同步更新此结论。
