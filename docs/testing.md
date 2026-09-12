# 测试方法

## 自动化

```sh
make test check
go test ./internal/app -run '^$' -fuzz FuzzSubscriptionParser -fuzztime 20s
make release
python3 tests/run_linux.py
python3 tests/run_linux.py --platform linux/amd64
python3 tests/run_linux.py --distro debian
python3 tests/run_linux.py --distro fedora
python3 tests/run_linux.py --distro arch --platform linux/amd64
```

Docker 需已经运行。原生 Linux 主机通常只支持自身架构；另一架构需要 Docker Desktop 或已配置的仿真支持。测试不会自行修改宿主 binfmt 配置。

单元测试覆盖格式识别、下载边界、配置管理字段、provider 文件快照、事务各阶段故障、竞态冲突、原代理恢复、路径安全、日志脱敏及私密 JSON 字段。

Linux 测试使用真实 systemd、mihomo、D-Bus/GSettings 和 KDE 工具。测试网络内提供订阅源、规则源、HTTP 目标、SOCKS5 TCP/UDP 代理及 DNS 服务，验证流量而不依赖公网节点。包含安装、API 认证、代理持久化、候选校验失败、原子重载、TUN、订阅切换、定时任务、异常重启和卸载。

`--live-stdin` 可从标准输入接收私密订阅 URL 的 JSON 数组。不要把真实 URL 放入命令参数、GitHub Actions、测试夹具或公共报告。真实源的测试输出只记录匿名序号与结果。

## 隔离与清理

每次执行建立独立容器、Docker 网络、镜像和 BuildKit 构建器。TUN 需要特权，但网络和 cgroup namespace 独立；不使用宿主网络，也不绑定宿主 cgroup 或 Docker socket。

测试退出时清理本次资源和临时目录，并确认执行前的资源仍存在。不会执行 `docker system prune`。`--image` 可以复用调用者提供的测试镜像；这类镜像由调用者负责最后删除，便于连续开发验证。

所有失败也必须通过清理核验；缺少 systemd/TUN 或外部网络不可达时不能将该项写成通过。

具体执行记录将在完成验证后记录到 `docs/test-results.md`。
