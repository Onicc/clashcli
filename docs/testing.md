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

代理环境选择用独立子进程验证，避免 Go 的环境缓存影响用例；包含大小写优先级、NO_PROXY、回环绕过和实际 HTTP 转发请求。另覆盖截断/超时的三次尝试、不接受部分响应、进度/原因脱敏、API 直连及 sudo 环境白名单。

可在真实 Linux 网络下以普通用户单独验证在线组件安装：

```sh
CLASHCLI_TEST_LIVE_DOWNLOAD=1 go test ./internal/app -run '^TestLiveComponentInstall$' -v -count=1 -timeout=16m
```

需要代理时先设置 `https_proxy` / `http_proxy`。该用例实际下载并校验 mihomo、MetaCubeXD、Geo，在私有临时目录中安装，并使用本地测试订阅启动候选内核检查 Unix API。不会安装系统服务、读取真实订阅、写入系统配置或开启代理/TUN；退出时终止候选进程并清理文件。也可交叉编译 `go test -c` 后上传到同架构 Linux 执行，上传的测试程序需自行清理。默认 `make test` 跳过该联网用例。

默认在线夹具和 Docker 完整订阅均包含 `IP-ASN` 规则，检查 ASN 数据已安装；单元测试另验证摘要错误/缺失资源不落盘、旧三文件安装补齐、候选目录包含 ASN 链接、进程退出原因及超时日志。Docker 还会移除测试容器里的 ASN 文件并验证下次候选预检补齐，不依赖 mihomo 偷偷下载成功而掩盖遗漏。

经授权使用真实订阅时，可额外设置 `CLASHCLI_TEST_LIVE_SUB_STDIN=1`，向编译后的测试程序标准输入提供 URL（例如从用户自行准备的 `0600` 文件重定向，勿放入参数或仓库）。该模式在临时回环端口启动专用内核，关闭 DNS 监听，使用首个内联节点请求公开 HTTPS 204 测试端点，然后进行原子代次切换、API 重载与健康检查。不会操作正式服务、系统代理、DNS 或 TUN；所有数据、进程和监听在退出时清理。该模式不在公共 CI 中使用，也不输出 URL、原始订阅或节点凭据。

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
