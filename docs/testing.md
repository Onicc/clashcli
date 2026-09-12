# 测试方法

## 自动化

```sh
make test check
make audit                  # 固定版本 govulncheck；需访问官方漏洞数据库
python3 tests/test_install.py # 离线安装器成功/失败用例；不写入宿主系统目录
go test ./internal/app -run '^$' -fuzz FuzzSubscriptionParser -fuzztime 20s
make release
python3 tests/run_linux.py
python3 tests/run_linux.py --platform linux/amd64
python3 tests/run_linux.py --distro debian
python3 tests/run_linux.py --distro fedora
python3 tests/run_linux.py --distro arch --platform linux/amd64
python3 tests/run_linux.py --coverage
```

Docker 需已经运行。原生 Linux 主机通常只支持自身架构；另一架构需要 Docker Desktop 或已配置的仿真支持。测试不会自行修改宿主 binfmt 配置。

单元测试覆盖格式识别、下载边界、配置管理字段、provider 文件快照、事务各阶段故障、竞态冲突、原代理恢复、路径安全、日志脱敏及私密 JSON 字段。

Linux 测试使用真实 systemd、mihomo、D-Bus/GSettings 和 KDE 工具。测试网络内提供订阅源、规则源、HTTP 目标、SOCKS5 TCP/UDP 代理及 DNS 服务，验证流量而不依赖公网节点。包含安装、API 认证、代理持久化、候选校验失败、原子重载、TUN、订阅切换、定时任务、异常重启和卸载。

每个 Linux 容器先执行真实 HTTPS 发行版安装：固定版本、普通用户 sudo 升级、旧文件描述符仍可用、权限与临时文件清理；此步骤需要能访问 GitHub Releases。之后替换为本次构建的 CLI 执行功能回归。安装器离线测试另覆盖篡改、错误清单、下载/权限/写入/重命名失败与旧程序保留。

Fedora 容器的 sudo 用例需兼容 [Ubuntu 宿主 AppArmor 的已知 unix-chkpwd 限制](https://gitlab.com/apparmor/apparmor/-/issues/402)：仅在该临时用例内，将容器的 `/etc/shadow` 从 `0000` 改为 root 专用的 `0600`，随后恢复原权限。不会修改宿主 AppArmor、PAM 策略或实际用户系统；正式安装脚本也没有这项操作。完整代理回归在恢复 Fedora 原权限后执行。

还覆盖真实 TUN 权限失败回滚、IPv6、SSH 转发、GNOME 感知型 GIO 客户端、并发更新及保留配置重装。`--coverage` 构建临时插桩 CLI，汇总真实 Linux 命令的代码覆盖率，然后删除二进制和覆盖率数据。

人工浏览器验收可使用 `--ui-review`，仅将测试夹具的面板临时映射到本机回环端口 9090。验证结束后在本次 subject 容器中执行 `touch /run/clashcli/ui-review-done` 继续测试；最多等待 10 分钟，随后关闭桥接服务。此阶段在任何真实订阅导入之前运行。

`--live-stdin` 可从标准输入接收私密订阅 URL 的 JSON 数组。不要把真实 URL 放入命令参数、GitHub Actions、测试夹具或公共报告。真实源的测试输出只记录匿名序号与结果。

## 隔离与清理

每次执行建立独立容器、Docker 网络、镜像和 BuildKit 构建器。TUN 需要特权，但网络和 cgroup namespace 独立；不使用宿主网络，也不绑定宿主 cgroup 或 Docker socket。

测试退出时清理本次资源和临时目录，并确认执行前的资源仍存在。不会执行 `docker system prune`。`--image` 可以复用调用者提供的测试镜像；这类镜像由调用者负责最后删除，便于连续开发验证。

所有失败也必须通过清理核验；缺少 systemd/TUN 或外部网络不可达时不能将该项写成通过。

具体执行记录见[验证记录](test-results.md)。
