# 提示词监控修复部署

仅替换 `workbuddy2api.service` 的二进制，不改配置和其他服务。归档增加兼容字段、分段文件名；回滚保留所有新归档，旧记录缺失的正文无法恢复。

先通过 Go 测试、race、vet、`python3 deploy/prompt-monitoring/test_release.py` 和合成浏览器验证。交叉编译 Linux amd64，校验文件 SHA256。服务器端为每次部署创建 `/opt/workbuddy2api-panel/backups/prompt-fix-<UTC>`（0700），放入 `release.py` 与 `workbuddy2api.next`。不要复用部署目录或覆盖备份。

`release.py prepare` 校验运行二进制、配置、磁盘余量和管理接口，复制原二进制/配置/归档并校验完整性。`check-recovery` 必须先作为独立 systemd 作业成功运行，证明 SSH 断开后仍可执行恢复程序并访问备份。

激活前创建独立 systemd 恢复 timer（`--on-active=300s --timer-property=AccuracySec=1s`），服务类型 `exec`、`RuntimeMaxSec=180`。timer 调用 `release.py recover`。激活作业同样由 systemd 执行，类型 `exec`、`RuntimeMaxSec=240`、`TimeoutStopSec=10`，调用 `release.py activate`。先验证 timer 为 active，再启动激活。

激活顺序：确认配置/二进制未变及 timer 已启用 → 停 workbuddy → 再备份停止瞬间的归档 → 原子替换二进制 → 启动 → 校验版本、密钥计数、提示词接口及鉴权 → 用一把已开启采集的受限 key 发送两条带明确会话 ID 的合成请求（模型白名单拒绝，无上游调用），验证归档 v2 和新增判定 → 写 `success.json` → 关闭 timer。合成自检会留两条标有自检内容的 403 记录。

激活失败立即恢复旧二进制；激活进程被中断/卡住时，timer 会停止激活作业并自主恢复。恢复先验证备份哈希，再恢复旧二进制、启动服务并验证旧版本。发布或恢复成功都会停用 timer；已有成功/恢复标记时，迟到的 timer 无操作。配置和归档始终保留当前状态；不得通过恢复旧目录覆盖新请求数据。发布后另行验证公网静态资源和鉴权、确认配置哈希一致、备份存在且权限正确、恢复 timer 已停用。

结果位于部署目录的 `manifest.json` / `success.json` / `recovered.json`；systemd 作业日志留在 journal。所有文件仅 root 可读，不把配置或归档内容加入 Git。线上私有路径及作业名另记本地 `data/prompt-release/`，不放入公共仓库。
