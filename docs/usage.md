# 高级使用

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

## 定时更新

```sh
clashcli sub schedule primary --every 12h
clashcli sub schedule primary --every 'Mon..Fri 09:00'
clashcli sub schedule primary --every off
clashcli sub update primary --force
```

每个订阅有独立 systemd timer。支持 `1h`、`6h`、`12h`、`daily`、`off` 和 systemd 日历表达式；采用系统时区，随机延迟最多 60 秒，停机错过的事件在启动后补执行一次。`--force` 忽略主订阅的 HTTP 缓存验证头。

即使主订阅返回 304，远程 provider 仍会重新检查。主配置和依赖都未变化时不重载。非活动订阅更新只更新其本地版本，切换时应用。

下载直接连接原始来源，正常校验 TLS；不会调用第三方转换服务、自动添加镜像或关闭证书校验。TUN 已开启时，普通出站下载也可能按当前 TUN 规则路由。

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

clashcli 输出日志会脱敏订阅 URL 查询参数和认证字段。原始订阅、生成配置、面板密钥、事务及代理恢复数据保存在 root 私有目录；不要直接公开这些文件或原始 journald 导出。

## 数据位置

| 位置 | 内容 |
|---|---|
| `/etc/clashcli` | 本机设置、订阅登记和密钥 |
| `/var/lib/clashcli` | 订阅版本、Geo 数据、UI、缓存及恢复记录 |
| `/run/clashcli` | 锁与临时预检进程资源 |
| `/usr/local/lib/clashcli` | 独立 mihomo 程序 |

卸载默认保留前两个目录，`--purge` 删除它们。系统代理环境文件和桌面设置按恢复记录还原；用户随后修改的值会保留并提示。
