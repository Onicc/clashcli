# clashcli

在 Linux 命令行中管理 [mihomo](https://github.com/MetaCubeX/mihomo)：系统代理、TUN、订阅和 [MetaCubeXD](https://github.com/MetaCubeX/metacubexd) 面板。

## 安装与配置

需要 **systemd、sudo/root 权限、amd64 或 arm64 Linux**。支持 Debian、Ubuntu、Fedora、Arch。普通代理不需要 TUN 设备；TUN 需要 `/dev/net/tun` 和网络管理权限。

通过本仓库的安装脚本安装最新稳定版（需要 `curl` 7.71+ 和 `coreutils`）：

```sh
curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/Onicc/clashcli/main/install.sh | sh
clashcli init
clashcli
```

脚本自动识别 amd64/arm64，校验同一发行版本的 SHA-256 后，原子安装到 `/usr/local/bin/clashcli`；普通用户会按需请求 sudo。重复运行即可升级 CLI，失败保留旧程序，不修改订阅、代理设置或正在运行的内核。只安装程序，不自动运行初始化。

可先[查看安装脚本](install.sh)再执行；指定版本时，在末尾使用 `sh -s -- --version v0.1.1`。无法访问 GitHub 时，可从 [Releases](https://github.com/Onicc/clashcli/releases) 手动下载对应架构的程序与 `SHA256SUMS`，校验后执行 `sudo install -m 0755 clashcli-linux-amd64 /usr/local/bin/clashcli`。

也可在 Linux 上使用 Go 1.26.8+ 从源码安装：

```sh
git clone https://github.com/Onicc/clashcli.git
cd clashcli
make build
sudo make install
clashcli init
```

向导询问订阅名称、链接和更新周期，自动安装经过 SHA-256 校验的内核、UI 和规则数据。订阅链接隐藏输入。默认每 12 小时更新、内核开机启动，系统代理与 TUN 初始关闭。

下载或订阅校验失败后，修正链接并再次运行 `clashcli init` 即可继续；保留配置卸载后也使用同一命令恢复安装。

直接输入 `clashcli` 使用交互菜单；所有功能也有独立命令。

## 常用命令

```sh
clashcli proxy on             # 开启持久系统代理
clashcli proxy off            # 恢复原来的系统代理设置
clashcli tun on               # 开启 TUN 流量接管
clashcli tun off
clashcli status               # 设置与实际状态
clashcli logs -f              # 实时日志
clashcli sub add backup       # 添加订阅，交互输入链接
clashcli sub use backup       # 切换订阅
clashcli sub update           # 更新当前订阅
clashcli sub update --all     # 更新所有订阅
clashcli sub schedule --every 6h
clashcli sub schedule --every off
clashcli nodes               # 查看策略组
clashcli select              # 交互选择节点
clashcli rollback            # 回到前一个保留版本
clashcli stop                # 停止内核并恢复系统代理
clashcli start               # 启动并恢复保存的开关偏好
```

系统代理与 TUN 是独立开关。GNOME/KDE 使用桌面系统代理设置；服务器写入系统代理环境配置，后续登录会话中的程序可继承。应用需要支持对应的代理设置；已有进程通常需要重启，systemd 后台服务遵循自己的环境配置。需要接管不支持系统代理的程序时，开启 TUN。

支持完整 Clash/Mihomo YAML、节点 YAML、Base64 和常见分享链接订阅。可保存多个订阅，每次启用一个。完整 YAML 保留策略组与规则；只有节点的订阅默认使用“中国大陆和局域网直连，其余代理”。端口、控制接口与 TUN 始终由 clashcli 管理。

订阅更新会准备完整候选版本，验证主配置、节点和规则依赖，然后原子切换并重载；失败时恢复旧版本。日志与状态会显示更新失败和待恢复事务。

## Web 面板

```sh
clashcli ui
clashcli ui --show-secret
```

默认面板为 `http://127.0.0.1:9090/ui/`，API 为 `http://127.0.0.1:9090`。在面板中填入 API 地址和密钥。服务器可通过 SSH 转发访问：

```sh
ssh -L 9090:127.0.0.1:9090 user@server
```

节点选择会保存；面板中其他配置调整属于内核运行状态，下次 CLI 重载时使用 clashcli 保存的设置。

## 检查与卸载

```sh
clashcli doctor
clashcli doctor --repair      # 修复未完成事务
clashcli uninstall           # 卸载，保留订阅与配置
clashcli uninstall --purge   # 卸载并永久删除订阅与配置
```

卸载恢复 clashcli 接管前的代理值，保留用户后来作出的其他修改。

更多内容：[高级使用](docs/usage.md) · [设计与调研](docs/design.md) · [整体审查](docs/review.md) · [验证记录](docs/test-results.md) · [测试方法](docs/testing.md) · [第三方组件](THIRD_PARTY_NOTICES.md)

## 开发

```sh
make test check
make release
make e2e                     # 需要运行中的 Docker；测试后自动清理专用资源
```

clashcli 使用 MIT 许可证；独立下载的第三方组件保留各自许可证。
