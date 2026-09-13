# 高级使用

## 安装与升级

```sh
curl -q -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/Onicc/clashcli/main/install.sh | sh
# 固定版本（也可用于手动降级 CLI）：
curl -q -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/Onicc/clashcli/main/install.sh | sh -s -- --version v0.1.1
```

安装脚本只管理 `/usr/local/bin/clashcli`，不自动初始化、重启内核或升级独立组件，不覆盖订阅、配置和开关偏好。下载失败、校验失败或替换失败时保留旧 CLI。脚本拒绝覆盖同名符号链接、目录或设备文件；请先自行确认旧安装来源。

默认通过 GitHub 最新稳定版重定向确定唯一版本，然后下载该版本的程序与校验清单。全程要求 HTTPS；SHA-256 用于完整性校验，不是独立数字签名，仍需信任本仓库及 GitHub 发布来源。可先下载并审阅脚本，也可使用固定 Git 提交的 raw 链接获取脚本。

临时下载文件自动清理；最终在 `/usr/local/bin` 内生成 root 所有的候选文件，再次校验并检查版本后原子重命名，支持 `/tmp` 挂载为 `noexec` 的系统。不要在订阅更新或配置操作执行期间切换到不兼容的旧版本。

## 非交互配置

订阅链接从标准输入读取，避免进入命令参数和 shell 历史。下面的文件由用户自行准备，权限应为 `0600`：

```sh
sudo clashcli init --name primary --url-stdin < subscription-url.txt
sudo clashcli sub edit primary --url-stdin < subscription-url.txt
sudo clashcli sub add backup --url-stdin < subscription-url.txt
sudo clashcli status --json
sudo clashcli sub list --json
```

没有终端且参数不足时，命令返回非零退出码，不等待交互输入。普通用户的交互调用会按需请求 sudo；不会配置免密码 sudo。

`init --mixed-port 7890 --controller-port 9090` 设置端口。端口必须不同，范围为 1024–65535。`--desktop gnome|kde|none` 可覆盖桌面自动检测，例如从 SSH 管理桌面会话；默认识别调用者的 `XDG_CURRENT_DESKTOP` 与 `SUDO_UID`。

## 离线资源

```sh
sudo clashcli init --name primary --url-stdin \
  --mihomo-file ./mihomo-linux-arm64.gz \
  --ui-file ./compressed-dist.tgz \
  --geo-dir ./geo < subscription-url.txt
```

`geo` 目录包含 `geoip.dat`、`GeoSite.dat`、`geoip.metadb`。本地导入代表用户信任这些文件；内核执行与配置仍会预检。在线安装固定 mihomo v1.19.30、MetaCubeXD v1.273.1 并验证内置摘要；Geo 数据验证官方发行元数据中的摘要。

`ui update` 重新安装当前 clashcli 版本验证过的 UI 基线，不跟踪不固定的上游分支。内核与 UI 的版本基线通过 clashcli 源码发布更新，避免未经验证的自动内核升级。

## 下载代理

v0.1.2 起，内核、UI、Geo、订阅和 provider 下载遵循进程的 `https_proxy` / `http_proxy` / `no_proxy`，也支持大写形式；同名大小写同时存在时大写优先。未设置时直连，`no_proxy` 匹配的目标与回环目标绕过代理。只设置 `all_proxy` 不生效；需要 SOCKS 时，可将 `https_proxy` / `http_proxy` 设置为 `socks5://host:port`。

```sh
# 使用已有可达的代理，不要求 clashcli 自己的内核已运行：
export https_proxy=http://127.0.0.1:7890 http_proxy=http://127.0.0.1:7890
clashcli init
clashcli sub update
# 手动 sudo / 非交互调用时，显式保留这些变量：
sudo --preserve-env=http_proxy,https_proxy,no_proxy,HTTP_PROXY,HTTPS_PROXY,NO_PROXY clashcli sub update
```

普通用户交互调用的自动 sudo 会保留上述变量（仍遵循本机 sudo 策略），不会保留全部环境。内核控制 API 和候选 Unix socket 始终直连；代理地址与凭据不会写入 clashcli 配置。这里的代理仅控制下载出口，不会自动切换系统代理或 TUN。不要指向尚未启动的本机代理；如果大小写变量冲突，请先清除旧值。

每次下载最多 5 分钟，连接/TLS/响应头另有限时，网络错误最多尝试 3 次；失败提示保留已收字节数和超时/连接中断原因，仍会脱敏 URL。不会关闭 TLS 验证或跳过摘要校验。

systemd 定时任务不会继承当前 SSH/shell 的临时 `export`。若定时更新也依赖已有代理，可运行 `sudo systemctl edit clashcli-update@.service` 配置：

```ini
[Service]
Environment="https_proxy=http://127.0.0.1:7890"
Environment="http_proxy=http://127.0.0.1:7890"
Environment="no_proxy=localhost,127.0.0.1,::1"
```

保存后运行 `sudo systemctl daemon-reload`，后续更新使用该环境；目标代理必须在任务运行时可用。这是用户自行管理的 systemd 覆盖配置，卸载 clashcli 不会删除它；不再需要时删除自己添加的条目并重新加载。不要在可公开读取的 unit 覆盖文件中存放代理密码。

## 定时更新

```sh
clashcli sub schedule primary --every 12h
clashcli sub schedule primary --every 'Mon..Fri 09:00'
clashcli sub schedule primary --every off
clashcli sub update primary --force
```

每个订阅有独立 systemd timer。支持 `1h`、`6h`、`12h`、`daily`、`off` 和 systemd 日历表达式；采用系统时区，随机延迟最多 60 秒，停机错过的事件在启动后补执行一次。`--force` 忽略主订阅的 HTTP 缓存验证头。

即使主订阅返回 304，远程 provider 仍会重新检查。主配置和依赖都未变化时不重载。非活动订阅更新只更新其本地版本，切换时应用。

下载从原始来源获取内容，按上述下载代理设置选择出口并正常校验 TLS；不会调用第三方转换服务、自动添加镜像或关闭证书校验。TUN 已开启时，普通出站下载也可能按当前 TUN 规则路由。

## 配置边界

完整订阅支持节点、策略组、规则、子规则、proxy/rule providers、DNS、hosts 和嗅探配置。客户端专用字段不进入内核配置。端口、监听地址、控制接口、密钥、UI、TUN、Geo 数据模式和选择缓存由本机管理。

远程 provider 接受 HTTP(S) 或 inline；HTTP provider 会转为本地文件快照。远程配置不能指定任意本地 file provider。节点 URI 的支持范围遵循固定版本 mihomo；未知格式或解析后丢失节点会拒绝更新。

clashcli 不会覆盖 `/etc/resolv.conf`，也不会替其他系统服务写入代理配置。系统代理环境变量的修改不能回写已经运行的父进程环境，因此不要求用户使用 `eval` 或 shell 函数来驱动系统开关。

## 恢复与日志

```sh
clashcli logs -n 100 --since '1 hour ago'
clashcli doctor --repair
clashcli rollback
```

配置文件切换与内核重载无法构成同一个操作系统事务。clashcli 用持久记录串联它们：未提交的事务恢复旧版本，已提交的事务恢复新版本。`status` 同时显示保存偏好、实际内核状态和是否需要恢复。

服务未运行时，更新仅提交下次启动使用的版本，不会意外启动代理。停止服务会撤销系统代理的实际设置，保留偏好；下一次启动恢复偏好。

`doctor` 检查实际节点/规则加载、代理监听、TUN 及系统代理偏差；不健康或服务已停止时返回非零退出码，适合脚本检查。没有 TUN 设备但 TUN 偏好关闭时只提示，不判定普通代理不可用。`doctor --repair` 可能启动服务，并按保存偏好恢复系统代理。

clashcli 输出日志会脱敏 URL 中的路径、查询参数、片段、用户信息及常见认证字段，只保留主机便于定位；过长日志行会报错退出而不是挂起。原始订阅、生成配置、面板密钥、事务及代理恢复数据保存在 root 私有目录；不要直接公开这些文件或原始 journald 导出。脱敏不能替代分享日志前的人工检查。

## 数据位置

| 位置 | 内容 |
|---|---|
| `/etc/clashcli` | 本机设置、订阅登记和密钥 |
| `/var/lib/clashcli` | 订阅版本、Geo 数据、UI、缓存及恢复记录 |
| `/run/clashcli` | 锁与临时预检进程资源 |
| `/usr/local/lib/clashcli` | 独立 mihomo 程序 |

卸载默认保留前两个目录，`--purge` 删除它们。系统代理环境文件和桌面设置按恢复记录还原；用户随后修改的值会保留并提示。
