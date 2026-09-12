# 设计与调研

## 选型

clashcli 是 Go 命令行管理程序，mihomo 是独立 systemd 进程，MetaCubeXD 是由内核提供的静态页面。CLI 与内核通过回环 HTTP API 通信，候选预检通过私有目录中的 Unix socket 通信。CLI 不链接 mihomo，不实现自己的代理协议栈或订阅转换服务。

调研基线为 mihomo v1.19.30、MetaCubeXD v1.273.1。上游事实与对应设计：

| 上游能力或限制 | clashcli 的实现 |
|---|---|
| [mihomo API](https://wiki.metacubex.one/api/) 支持重载及运行状态查询 | 重载后检查端口、TUN、节点和规则，不只检查 HTTP 状态码 |
| [ApplyConfig](https://github.com/MetaCubeX/mihomo/blob/v1.19.30/hub/executor/executor.go) 对部分运行错误记录日志 | 候选内核完整初始化预检，加生效后的检查与回滚 |
| [file provider 原生转换](https://github.com/MetaCubeX/mihomo/blob/v1.19.30/adapter/provider/provider.go) 支持节点订阅 | 通用订阅使用本地 file provider，并核对 URI 与解析节点数量 |
| [Linux 系统代理适配](https://github.com/clash-verge-rev/sysproxy-rs/blob/main/src/linux.rs) 使用 GNOME/KDE | 分别管理桌面设置，额外提供服务器的持久环境配置 |
| [TUN](https://wiki.metacubex.one/config/inbound/tun/) 能自动管理路由与重定向 | 使用独立 TUN 开关，检查设备与实际流量 |
| [MetaCubeXD](https://github.com/MetaCubeX/metacubexd/releases/tag/v1.273.1) 提供静态 tgz | 校验、受限解压，通过 external-ui 提供页面 |

Go 负责类型化状态与事务；Cobra 负责子命令、参数和补全；YAML v3 使用 YAML 组织维护的发行包，保持与内核配置格式的兼容性。终端菜单采用编号输入，无常驻 TUI 状态机。

## 配置与更新

登记配置、订阅原文和生成的内核配置分离。完整 YAML 保留支持的业务字段；本机网络与管理字段统一覆盖。HTTP providers 被下载为同版本目录下的文件，生成的引用使用确定的本地路径。

```text
下载订阅及依赖 → 生成候选目录 → 语法与候选内核预检
                                      ↓
                       加生效锁并复核登记版本
                                      ↓
持久事务记录 → 原子切换 current → 内核重载 → 生效检查 → 提交
                                      ↓ 失败
                           恢复旧目录、登记与内核配置
```

文件写入使用同目录临时文件、fsync、rename 和目录同步。订阅下载持有该订阅的锁；最终替换持有全局生效锁。提交前检查配置修订号，冲突时保留原状态并要求重试，避免覆盖同时发生的本地设置修改。

事务记录包含切换前后的路径和登记配置。进程或机器在提交前中断时，恢复旧状态；提交后中断时，完成新状态。服务启动前执行离线恢复；运行中的修复由 `doctor --repair` 执行。只把确认成功的版本纳入历史回滚。

旧版本热重载也失败时，先持久恢复旧配置、释放生效锁，再重启内核并复核，避免与 systemd 启动前恢复发生锁互相等待。候选 API 开始监听不等于初始化完成，必须等待声明的节点、provider 和规则均已出现。

主订阅的 HTTP 304 不代表它引用的规则集未变化，因此仍检查依赖。内容相同时只记录检查时间。失败会记录脱敏错误，不修改活动订阅的节点和规则。

规则 YAML 会规范化为多行列表，以适配上游[流式规则读取器](https://github.com/MetaCubeX/mihomo/blob/v1.19.30/rules/provider/provider.go)。provider 请求支持自定义 header，但跨来源重定向不携带这些 header。远程节点不允许引用本机证书或私钥文件。

## 权限与生命周期

控制接口和代理端口默认仅绑定回环地址，API 使用随机密钥。设置、订阅版本和恢复记录默认仅 root 可读。候选内核关闭 TUN、代理入口和后台健康检测，不接管主机网络。

systemd 管理内核重启，主进程限制网络能力与可写目录。配置恢复与桌面代理操作通过独立启动/停止辅助命令执行，桌面命令以原用户身份运行。系统代理保存原值，恢复时进行所有权比较，避免覆盖用户后续更改。

TUN 状态由生成配置持久化；网卡与路由由 mihomo 管理。UI 的节点选择由内核缓存，其他 UI 运行时设置不修改 clashcli 的持久登记。

## 兼容性边界

首版要求 systemd，不支持 OpenRC、OpenWrt 或无 systemd 的普通容器作为产品安装目标。Docker 仅作为测试 Linux 环境，测试容器运行真实 systemd。Linux 系统代理不能强制所有程序遵循代理配置；TUN 用于流量接管。

内核、UI 和公共 Geo 数据独立于订阅事务管理。订阅事务覆盖主配置及其 provider 文件，不承诺跨进程的零延迟切换或连接完全不中断。
