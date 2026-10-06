# Astra HUD 集成

Astra Default HUD 1.0.0（安装包作者 Hyun-05）默认随 Replay 可执行文件分发；入口 `/astra/index.html`，OBS 源 `Project Replay Astra HUD`。不依赖第三方 Socket.IO、独立 Node 服务或本地 ZIP 目录。菜单中的 OpenHUD 使用已有 OBS 源（默认 `OpenHUD Broadcast`），保持独立软件兼容；基本战队 HUD、ZIP、URL 和其他源仍可选择。既有用户明确选择不自动覆盖。

## GSI 配置与安装包的对应关系

| 安装包功能 | GSI 订阅 |
| --- | --- |
| 游戏与地图、双方比分 | provider、map、round |
| 回合胜负历史 | map_round_wins |
| 回合/冻结/暂停/C4 倒计时 | phase_countdowns |
| 当前观战玩家、血量护甲、统计 | player_id、player_state、player_match_stats |
| 当前武器、弹药与位置/朝向 | player_weapons、player_position |
| 十人卡片、经济、K/D/A | allplayers_id、allplayers_state、allplayers_match_stats |
| 全员武器、投掷物库存 | allplayers_weapons |
| 雷达点位和朝向 | allplayers_position |
| C4 位置与安拆包状态 | bomb |
| 手雷轨迹、烟雾与燃烧区域 | allgrenades（HUD 同时兼容 grenades/allgrenades 响应） |

输出精度为时间 3 位、位置 1 位、向量 3 位；buffer 0.05 秒、throttle 0.1 秒、heartbeat 1 秒。启动时自动写入 Replay 自有 A/B 配置；下载配置使用同一个生成器，不更改其他 HUD 的 GSI 配置。配置更新后需重启 CS2，全员与位置字段要求观战/GOTV 权限。C4 和手雷字段是否出现取决于当前游戏状态。

HUD 通过只读 `/hud-data` 接收实时状态；离线或数据过期会隐藏旧画面。HTML 运行在隔离 sandbox 中，无法修改 Replay 控制 API。GSI 的认证字段不向 HUD 暴露。

使用安装包原始视觉布局、内置雷达、头像和武器资源。战队名称和双方身份来自当前 GSI，避免半场换边后错误匹配；安装包原先依赖第三方软件的自定义头像、赛程/BO 系列信息和布局编辑服务不由 GSI 提供。该安装包声明 `killfeed: false`，不包含独立击杀提示组件。

参考配置：https://github.com/JoshuaJKrueger/cs2_gsi/blob/master/gamestate_integration_example.cfg
