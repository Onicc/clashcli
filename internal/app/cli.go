package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var errElevated = errors.New("completed through sudo")
var inputReader = bufio.NewReader(os.Stdin)

func interactive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func sudoArguments(exe string, args []string) []string {
	// Preserve only the desktop hint and standard download-proxy variables, not
	// the entire user environment or executable/library search paths.
	return append([]string{"--preserve-env=XDG_CURRENT_DESKTOP,http_proxy,https_proxy,no_proxy,HTTP_PROXY,HTTPS_PROXY,NO_PROXY", "--", exe}, args...)
}

func prompt(label, def string, secret bool) (string, error) {
	if !interactive() {
		return "", fmt.Errorf("缺少 %s；非交互模式请提供参数", label)
	}
	fmt.Print(label)
	if def != "" {
		fmt.Printf(" [%s]", def)
	}
	fmt.Print(": ")
	var value string
	var err error
	if secret {
		var b []byte
		b, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		value = string(b)
	} else {
		value, err = inputReader.ReadString('\n')
	}
	if err != nil {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = def
	}
	return value, nil
}
func Execute(version string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	root := newCommand(NewPaths("/"), version)
	err := root.ExecuteContext(ctx)
	if errors.Is(err, errElevated) {
		return nil
	}
	return err
}
func newCommand(p Paths, version string) *cobra.Command {
	root := &cobra.Command{Use: "clashcli", Short: "Linux 命令行代理管理（mihomo）", SilenceErrors: true, SilenceUsage: true, Version: version}
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Name() == "completion" || cmd.Parent() != nil && cmd.Parent().Name() == "completion" {
			return nil
		}
		if runtime.GOOS != "linux" {
			return errors.New("clashcli 的系统管理功能仅支持 Linux")
		}
		if os.Geteuid() != 0 {
			if !interactive() {
				return errors.New("需要管理员权限；请通过 sudo 运行此命令")
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			args := sudoArguments(exe, os.Args[1:])
			child := exec.CommandContext(cmd.Context(), "sudo", args...)
			child.Stdin = os.Stdin
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err = child.Run(); err != nil {
				return err
			}
			return errElevated
		}
		return p.Ensure()
	}
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		if _, err := os.Stat(p.Settings()); errors.Is(err, os.ErrNotExist) {
			return initialize(cmd.Context(), p, initOptions{})
		}
		return menu(cmd.Context(), p)
	}
	var opts initOptions
	initCmd := &cobra.Command{Use: "init", Short: "交互初始化（也可提供参数自动安装）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return initialize(cmd.Context(), p, opts) }}
	initCmd.Flags().StringVar(&opts.Name, "name", "", "订阅名称")
	initCmd.Flags().BoolVar(&opts.URLStdin, "url-stdin", false, "从标准输入读取订阅链接")
	initCmd.Flags().StringVar(&opts.CoreFile, "mihomo-file", "", "导入本地 Linux 内核（可信来源）")
	initCmd.Flags().StringVar(&opts.UIFile, "ui-file", "", "导入本地静态 UI tgz（可信来源）")
	initCmd.Flags().StringVar(&opts.GeoDir, "geo-dir", "", "导入本地 Geo 数据目录（可信来源）")
	initCmd.Flags().BoolVar(&opts.NoStart, "no-start", false, "完成配置后不启动服务")
	initCmd.Flags().BoolVar(&opts.NoTimer, "no-timer", false, "不启用订阅定时更新")
	initCmd.Flags().StringVar(&opts.Desktop, "desktop", "", "桌面适配：gnome、kde、none（默认自动识别）")
	initCmd.Flags().IntVar(&opts.MixedPort, "mixed-port", 7890, "混合代理端口")
	initCmd.Flags().IntVar(&opts.ControllerPort, "controller-port", 9090, "控制接口端口")
	root.AddCommand(initCmd)
	for _, verb := range []string{"start", "stop", "restart"} {
		v := verb
		root.AddCommand(&cobra.Command{Use: v, Short: map[string]string{"start": "启动内核", "stop": "停止内核并恢复系统代理", "restart": "重启内核"}[v], Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			switch v {
			case "start":
				return startService(cmd.Context(), p)
			case "stop":
				return stopService(cmd.Context(), p)
			default:
				if err := stopService(cmd.Context(), p); err != nil {
					return err
				}
				return startService(cmd.Context(), p)
			}
		}})
	}
	for _, kind := range []string{"proxy", "tun"} {
		k := kind
		root.AddCommand(&cobra.Command{Use: k + " <on|off|status>", Short: map[string]string{"proxy": "持久系统代理开关", "tun": "TUN 流量接管开关"}[k], Args: cobra.ExactArgs(1), ValidArgs: []string{"on", "off", "status"}, RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] == "status" {
				return printStatus(cmd.Context(), p, false)
			}
			if args[0] != "on" && args[0] != "off" {
				return errors.New("参数必须为 on、off 或 status")
			}
			if k == "proxy" {
				return changeProxy(cmd.Context(), p, args[0] == "on")
			}
			return changeTun(cmd.Context(), p, args[0] == "on")
		}})
	}
	var jsonStatus bool
	status := &cobra.Command{Use: "status", Short: "查看设置与实际运行状态", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return printStatus(cmd.Context(), p, jsonStatus) }}
	status.Flags().BoolVar(&jsonStatus, "json", false, "输出 JSON")
	root.AddCommand(status)
	var follow bool
	var since string
	var lines int
	logs := &cobra.Command{Use: "logs", Short: "查看内核与更新日志", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return showLogs(cmd.Context(), follow, since, lines) }}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "持续跟随")
	logs.Flags().StringVar(&since, "since", "", "开始时间（journalctl 格式）")
	logs.Flags().IntVarP(&lines, "lines", "n", 100, "最近行数")
	root.AddCommand(logs)
	root.AddCommand(subscriptionCommand(p))
	root.AddCommand(&cobra.Command{Use: "nodes", Short: "查看策略组和节点", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return listNodes(cmd.Context(), p) }})
	root.AddCommand(&cobra.Command{Use: "select [group] [node]", Short: "交互选择代理节点", Args: cobra.MaximumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error { return selectNode(cmd.Context(), p, args) }})
	var showSecret bool
	ui := &cobra.Command{Use: "ui", Short: "显示 Web UI 入口", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := loadSettings(p)
		if err != nil {
			return err
		}
		fmt.Printf("UI: http://127.0.0.1:%d/ui/\nAPI: http://127.0.0.1:%d\n", s.ControllerPort, s.ControllerPort)
		if showSecret {
			fmt.Println("Secret:", s.Secret)
		} else {
			fmt.Println("面板连接密钥：clashcli ui --show-secret")
		}
		return nil
	}}
	ui.Flags().BoolVar(&showSecret, "show-secret", false, "显式显示本机面板密钥")
	ui.AddCommand(&cobra.Command{Use: "update", Short: "重新安装经过校验的 UI 基线版本", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return installUI(cmd.Context(), p, "") }})
	root.AddCommand(ui)
	var repair bool
	doctor := &cobra.Command{Use: "doctor", Short: "检查环境与未完成事务", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return doctor(cmd.Context(), p, repair) }}
	doctor.Flags().BoolVar(&repair, "repair", false, "恢复未完成事务并修复代理状态")
	root.AddCommand(doctor)
	var purge, yes bool
	uninstall := &cobra.Command{Use: "uninstall", Short: "卸载并恢复原代理设置（默认保留订阅）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !yes {
			value, err := prompt("卸载 clashcli？输入 yes 确认", "no", false)
			if err != nil {
				return err
			}
			if value != "yes" {
				return errors.New("已取消")
			}
		}
		if err := removeInstallation(cmd.Context(), p, purge); err != nil {
			return err
		}
		if purge {
			fmt.Println("已卸载并删除订阅、版本和配置（不可恢复）")
		} else {
			fmt.Println("已卸载，订阅和配置保留在 /etc/clashcli 与 /var/lib/clashcli")
		}
		return nil
	}}
	uninstall.Flags().BoolVar(&purge, "purge", false, "同时永久删除订阅与配置")
	uninstall.Flags().BoolVarP(&yes, "yes", "y", false, "确认卸载")
	root.AddCommand(uninstall)
	root.AddCommand(&cobra.Command{Use: "rollback", Short: "回滚活动订阅到前一个保留版本", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return rollbackPrevious(cmd.Context(), p) }})
	internal := &cobra.Command{Use: "internal", Hidden: true}
	internal.AddCommand(&cobra.Command{Use: "recover", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := loadSettings(p)
		if err != nil {
			return err
		}
		return (Manager{P: p, Core: RealCore{p, s}}).Recover(cmd.Context(), true)
	}})
	internal.AddCommand(&cobra.Command{Use: "proxy-clear", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		unlock, err := lock(cmd.Context(), filepath.Join(p.Run, "proxy.lock"))
		if err != nil {
			return err
		}
		defer unlock()
		return disableProxy(cmd.Context(), p)
	}})
	internal.AddCommand(&cobra.Command{Use: "proxy-restore", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := loadSettings(p)
		if err != nil {
			return err
		}
		var g Generation
		if err = readJSON(filepath.Join(p.Current(), "generation.json"), &g); err != nil {
			return err
		}
		if err = (RealCore{p, s}).Check(cmd.Context(), g); err != nil {
			return err
		}
		if s.Proxy {
			unlock, err := lock(cmd.Context(), filepath.Join(p.Run, "proxy.lock"))
			if err != nil {
				return err
			}
			defer unlock()
			return enableProxy(cmd.Context(), p, s)
		}
		return nil
	}})
	root.AddCommand(internal)
	return root
}

type initOptions struct {
	Name                              string
	URLStdin                          bool
	CoreFile, UIFile, GeoDir, Desktop string
	NoStart, NoTimer                  bool
	MixedPort, ControllerPort         int
}

func readURL(stdin bool) (string, error) {
	if stdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<10+1))
		if err != nil {
			return "", err
		}
		if len(b) > 16<<10 {
			return "", errors.New("链接过长")
		}
		return strings.TrimSpace(string(b)), nil
	}
	return prompt("订阅链接", "", true)
}
func initialize(ctx context.Context, p Paths, o initOptions) error {
	if _, err := os.Stat(p.Settings()); err == nil {
		return resumeInstallation(ctx, p, o)
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("需要正在运行的 systemd；请在支持的 Linux 主机上使用")
	}
	if o.MixedPort == 0 {
		o.MixedPort = 7890
	}
	if o.ControllerPort == 0 {
		o.ControllerPort = 9090
	}
	if o.MixedPort < 1024 || o.MixedPort > 65535 || o.ControllerPort < 1024 || o.ControllerPort > 65535 || o.MixedPort == o.ControllerPort {
		return errors.New("端口必须不同且位于 1024–65535")
	}
	if o.Name == "" {
		var err error
		o.Name, err = prompt("订阅名称", "primary", false)
		if err != nil {
			return err
		}
	}
	u, err := readURL(o.URLStdin)
	if err != nil {
		return err
	}
	if err = validateSubInput(o.Name, u); err != nil {
		return err
	}
	s := DefaultSettings()
	s.MixedPort = o.MixedPort
	s.ControllerPort = o.ControllerPort
	s.DesktopUID, s.Desktop = originalUser()
	if o.Desktop != "" {
		s.Desktop = o.Desktop
	}
	if _, err = desktopValues(s); err != nil {
		return err
	}
	calendar := DefaultCalendar
	if interactive() {
		period, err := prompt("自动更新：12h / 6h / daily / off", "12h", false)
		if err != nil {
			return err
		}
		switch period {
		case "off":
			o.NoTimer = true
		case "12h", "6h", "daily":
		default:
			return errors.New("无效更新周期")
		}
		calendar = calendarAlias(period)
	}
	fmt.Println("安装 mihomo…")
	if err = installCore(ctx, p, o.CoreFile); err != nil {
		return err
	}
	fmt.Println("安装 MetaCubeXD…")
	if err = installUI(ctx, p, o.UIFile); err != nil {
		return err
	}
	if o.GeoDir != "" {
		for _, name := range []string{"geoip.dat", "GeoSite.dat", "geoip.metadb"} {
			if err = copyFile(filepath.Join(o.GeoDir, name), filepath.Join(p.Data, name), 0600); err != nil {
				return err
			}
		}
	} else {
		fmt.Println("准备 Geo 数据…")
		if err = ensureGeo(ctx, p); err != nil {
			return err
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p.Executable()), 0755); err != nil {
		return err
	}
	if exe != p.Executable() {
		if err = copyFile(exe, p.Executable(), 0755); err != nil {
			return err
		}
	}
	sub := Subscription{ID: randomID(8), Name: o.Name, URL: u, Calendar: calendar}
	if o.NoTimer {
		sub.Calendar = ""
	}
	s.Active = sub.ID
	s.Subscriptions = []Subscription{sub}
	if err = saveSettings(p, s); err != nil {
		return err
	}
	fmt.Println("下载并预检订阅…")
	if err = updateSubscription(ctx, p, sub.ID, true); err != nil {
		return fmt.Errorf("订阅配置未完成；可用 sub edit / sub update 重试: %w", err)
	}
	if err = installService(ctx, p); err != nil {
		return err
	}
	if err = configureTimer(ctx, p, sub); err != nil {
		return err
	}
	if err = systemctl(ctx, "enable", "clashcli.service"); err != nil {
		return err
	}
	if !o.NoStart {
		if err = startService(ctx, p); err != nil {
			return err
		}
	}
	fmt.Println("配置完成。输入 clashcli 打开菜单；proxy on 开启系统代理，tun on 开启 TUN。")
	return printStatus(ctx, p, false)
}

// A failed first download and a non-purging uninstall must both be recoverable.
func resumeInstallation(ctx context.Context, p Paths, o initOptions) error {
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	fmt.Println("保留现有订阅与偏好，检查并恢复安装…")
	if _, err = os.Stat(p.Core()); err != nil || o.CoreFile != "" {
		if err = installCore(ctx, p, o.CoreFile); err != nil {
			return err
		}
	}
	if _, err = os.Stat(filepath.Join(p.Data, "ui", "current", "index.html")); err != nil || o.UIFile != "" {
		if err = installUI(ctx, p, o.UIFile); err != nil {
			return err
		}
	}
	if err = ensureGeo(ctx, p); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe != p.Executable() {
		if err = copyFile(exe, p.Executable(), 0755); err != nil {
			return err
		}
	}
	if err = (Manager{P: p, Core: RealCore{p, s}}).Recover(ctx, false); err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(p.Current(), "config.yaml")); err != nil {
		if err = updateSubscription(ctx, p, s.Active, true); err != nil {
			return err
		}
	}
	if err = installService(ctx, p); err != nil {
		return err
	}
	for _, sub := range s.Subscriptions {
		if err = configureTimer(ctx, p, sub); err != nil {
			return err
		}
	}
	if err = systemctl(ctx, "enable", "clashcli.service"); err != nil {
		return err
	}
	if !o.NoStart {
		if err = startService(ctx, p); err != nil {
			return err
		}
	}
	return printStatus(ctx, p, false)
}
func validateSubInput(name, raw string) error {
	if name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		return errors.New("订阅名称不能为空或包含换行，最长 80 字节")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || len(raw) > 16<<10 {
		return errors.New("请输入有效的 HTTP(S) 订阅链接")
	}
	return nil
}
func calendarAlias(v string) string {
	switch v {
	case "off":
		return ""
	case "12h":
		return DefaultCalendar
	case "6h":
		return "*-*-* 00,06,12,18:00:00"
	case "1h":
		return "hourly"
	case "daily":
		return "daily"
	default:
		return v
	}
}
func subscriptionCommand(p Paths) *cobra.Command {
	sub := &cobra.Command{Use: "sub", Short: "订阅管理"}
	var jsonList bool
	list := &cobra.Command{Use: "list", Short: "列出保存的订阅（隐藏链接）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := loadSettings(p)
		if err != nil {
			return err
		}
		if jsonList {
			return printJSON(s.Subscriptions)
		}
		for _, v := range s.Subscriptions {
			mark := " "
			if v.ID == s.Active {
				mark = "*"
			}
			fmt.Printf("%s %s  更新: %s  最近成功: %s\n", mark, v.Name, scheduleLabel(v.Calendar), timeLabel(v.LastUpdated))
		}
		return nil
	}}
	list.Flags().BoolVar(&jsonList, "json", false, "输出 JSON")
	sub.AddCommand(list)
	for _, action := range []string{"add", "edit"} {
		a := action
		var stdin bool
		cmd := &cobra.Command{Use: a + " [name]", Short: map[string]string{"add": "添加订阅", "edit": "修改订阅链接"}[a], Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			if name == "" {
				var err error
				name, err = prompt("订阅名称", "", false)
				if err != nil {
					return err
				}
			}
			u, err := readURL(stdin)
			if err != nil {
				return err
			}
			if err = validateSubInput(name, u); err != nil {
				return err
			}
			var changed Subscription
			err = mutateSettings(cmd.Context(), p, func(s *Settings) error {
				existing, e := s.Sub(name)
				if a == "add" {
					if e == nil {
						return errors.New("订阅名称已存在")
					}
					changed = Subscription{ID: randomID(8), Name: name, URL: u, Calendar: DefaultCalendar}
					s.Subscriptions = append(s.Subscriptions, changed)
				} else {
					if e != nil {
						return e
					}
					existing.URL = u
					existing.ETag = ""
					existing.Modified = ""
					changed = *existing
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err = configureTimer(cmd.Context(), p, changed); err != nil {
				return err
			}
			return updateSubscription(cmd.Context(), p, changed.ID, true)
		}}
		cmd.Flags().BoolVar(&stdin, "url-stdin", false, "从标准输入读取链接")
		sub.AddCommand(cmd)
	}
	sub.AddCommand(&cobra.Command{Use: "use <name>", Short: "切换活动订阅", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return useSubscription(cmd.Context(), p, args[0]) }})
	var id string
	var force, all bool
	update := &cobra.Command{Use: "update [name]", Short: "更新订阅，验证成功后原子替换", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if all {
			s, err := loadSettings(p)
			if err != nil {
				return err
			}
			errs := []error{}
			for _, v := range s.Subscriptions {
				if err = updateSubscription(cmd.Context(), p, v.ID, force); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", v.Name, err))
				}
			}
			return errors.Join(errs...)
		}
		name := id
		if len(args) > 0 {
			name = args[0]
		}
		return updateSubscription(cmd.Context(), p, name, force)
	}}
	update.Flags().BoolVar(&force, "force", false, "忽略 HTTP 缓存重新下载")
	update.Flags().BoolVar(&all, "all", false, "更新所有订阅")
	update.Flags().StringVar(&id, "id", "", "按内部 ID 更新")
	_ = update.Flags().MarkHidden("id")
	sub.AddCommand(update)
	sub.AddCommand(&cobra.Command{Use: "remove <name>", Short: "删除非活动订阅", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := loadSettings(p)
		if err != nil {
			return err
		}
		v, err := s.Sub(args[0])
		if err != nil {
			return err
		}
		if v.ID == s.Active {
			return errors.New("请先切换到其他订阅，再删除此活动订阅")
		}
		oldID := v.ID
		unlock, err := lock(cmd.Context(), filepath.Join(p.Run, "sub-"+oldID+".lock"))
		if err != nil {
			return err
		}
		defer unlock()
		v.Calendar = ""
		if err = configureTimer(cmd.Context(), p, *v); err != nil {
			return err
		}
		err = mutateSettings(cmd.Context(), p, func(current *Settings) error {
			if current.Active == oldID {
				return errors.New("订阅已被其他命令启用")
			}
			out := []Subscription{}
			for _, v := range current.Subscriptions {
				if v.ID != oldID {
					out = append(out, v)
				}
			}
			current.Subscriptions = out
			return nil
		})
		if err == nil {
			pruneGenerations(p)
		}
		return err
	}})
	var calendar string
	schedule := &cobra.Command{Use: "schedule [name]", Short: "设置定时更新（off / 1h / 6h / 12h / daily / 日历表达式）", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := loadSettings(p)
		if err != nil {
			return err
		}
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		v, err := s.Sub(name)
		if err != nil {
			return err
		}
		value := calendar
		if value == "" {
			value, err = prompt("更新周期", "12h", false)
			if err != nil {
				return err
			}
		}
		v.Calendar = calendarAlias(value)
		if err = configureTimer(cmd.Context(), p, *v); err != nil {
			return err
		}
		id := v.ID
		return mutateSettings(cmd.Context(), p, func(current *Settings) error {
			dest, err := current.Sub(id)
			if err != nil {
				return err
			}
			dest.Calendar = v.Calendar
			return nil
		})
	}}
	schedule.Flags().StringVar(&calendar, "every", "", "更新周期或 systemd 日历表达式")
	sub.AddCommand(schedule)
	return sub
}
func scheduleLabel(s string) string {
	if s == "" {
		return "关闭"
	}
	return s
}
func timeLabel(t time.Time) string {
	if t.IsZero() {
		return "尚无"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}
func printStatus(ctx context.Context, p Paths, jsonOutput bool) error {
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var conf any
	apiErr := APIFor(s).Do(probeCtx, "GET", "/configs", nil, &conf)
	proxy, proxyErr := proxyActual(probeCtx, p)
	if proxyErr != nil {
		proxy = map[string]any{"error": Redact(proxyErr.Error())}
	}
	current, _ := os.Readlink(p.Current())
	_, txErr := os.Stat(filepath.Join(p.Data, "transaction.json"))
	iface, ifaceErr := net.InterfaceByName("clashcli0")
	_ = iface
	state := map[string]any{"schema": 1, "core_running": apiErr == nil, "desired": map[string]any{"system_proxy": s.Proxy, "tun": s.Tun}, "system_proxy": proxy, "tun_interface": ifaceErr == nil, "runtime": conf, "generation": filepath.Base(current), "recovery_required": txErr == nil, "subscriptions": s.Subscriptions, "active_subscription": s.Active, "ui": fmt.Sprintf("http://127.0.0.1:%d/ui/", s.ControllerPort)}
	// /configs can include authentication fields; emit only fields that describe actual state.
	if runtimeConfig, ok := conf.(map[string]any); ok {
		state["runtime"] = map[string]any{"mixed_port": runtimeConfig["mixed-port"], "mode": runtimeConfig["mode"], "tun": runtimeConfig["tun"]}
	}
	if jsonOutput {
		return printJSON(state)
	}
	fmt.Printf("内核: %s\n系统代理: 保存=%t，实际=%v\nTUN: 保存=%t，网卡=%t\n", map[bool]string{true: "运行中", false: "未运行"}[apiErr == nil], s.Proxy, proxy, s.Tun, ifaceErr == nil)
	if v, e := s.Sub(s.Active); e == nil {
		fmt.Printf("订阅: %s\n最近更新: %s\n定时更新: %s\n", v.Name, timeLabel(v.LastUpdated), scheduleLabel(v.Calendar))
		if v.LastError != "" {
			fmt.Println("更新错误:", Redact(v.LastError))
		}
	}
	fmt.Printf("UI: %s\n", state["ui"])
	if txErr == nil {
		fmt.Println("检测到未完成事务：clashcli doctor --repair")
	}
	return nil
}
func showLogs(ctx context.Context, follow bool, since string, n int) error {
	if n < 0 || n > 100000 {
		return errors.New("日志行数需在 0–100000 之间")
	}
	args := []string{"--no-pager", "-u", "clashcli.service", "-u", "clashcli-update@*", "-n", strconv.Itoa(n), "-o", "short-iso"}
	if follow {
		args = append(args, "-f")
	}
	if since != "" {
		args = append(args, "--since", since)
	}
	c := exec.CommandContext(ctx, "journalctl", args...)
	pipe, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	c.Stderr = os.Stderr
	if err = c.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		fmt.Println(Redact(scanner.Text()))
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		// A scanner limit error stops draining stdout. Kill the producer before
		// waiting, otherwise journalctl can block forever on the full pipe.
		_ = c.Process.Kill()
	}
	err = c.Wait()
	if scanErr != nil {
		return scanErr
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type proxyEntry struct {
	Type string   `json:"type"`
	Now  string   `json:"now"`
	All  []string `json:"all"`
}

func getGroups(ctx context.Context, p Paths) (Settings, map[string]proxyEntry, error) {
	s, err := loadSettings(p)
	if err != nil {
		return s, nil, err
	}
	var result struct {
		Proxies map[string]proxyEntry `json:"proxies"`
	}
	err = APIFor(s).Do(ctx, "GET", "/proxies", nil, &result)
	return s, result.Proxies, err
}
func listNodes(ctx context.Context, p Paths) error {
	_, groups, err := getGroups(ctx, p)
	if err != nil {
		return err
	}
	names := []string{}
	for k, v := range groups {
		if len(v.All) > 0 {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		v := groups[name]
		fmt.Printf("%s [%s] 当前=%s\n", name, v.Type, v.Now)
		for _, node := range v.All {
			fmt.Printf("  %s\n", node)
		}
	}
	return nil
}
func selectNode(ctx context.Context, p Paths, args []string) error {
	s, groups, err := getGroups(ctx, p)
	if err != nil {
		return err
	}
	name, node := "", ""
	if len(args) > 0 {
		name = args[0]
	}
	if len(args) > 1 {
		node = args[1]
	}
	if name == "" {
		names := []string{}
		for k, v := range groups {
			if v.Type == "Selector" {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		name, err = choose("选择策略组", names)
		if err != nil {
			return err
		}
	}
	group, ok := groups[name]
	if !ok || group.Type != "Selector" {
		return errors.New("需要可手动选择的策略组")
	}
	if node == "" {
		node, err = choose("选择节点", group.All)
		if err != nil {
			return err
		}
	}
	valid := false
	for _, v := range group.All {
		if v == node {
			valid = true
		}
	}
	if !valid {
		return errors.New("节点不在该策略组中")
	}
	return APIFor(s).Do(ctx, "PUT", "/proxies/"+url.PathEscape(name), map[string]string{"name": node}, nil)
}
func choose(label string, values []string) (string, error) {
	if len(values) == 0 {
		return "", errors.New("没有可选项")
	}
	for i, v := range values {
		fmt.Printf("%d. %s\n", i+1, v)
	}
	raw, err := prompt(label, "1", false)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > len(values) {
		return "", errors.New("无效序号")
	}
	return values[n-1], nil
}
func doctor(ctx context.Context, p Paths, repair bool) error {
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	if repair {
		m := Manager{P: p, Core: RealCore{p, s}}
		if err = m.Recover(ctx, false); err != nil {
			// Stop first so ExecStartPre can perform offline transaction recovery without a live API.
			if e := systemctl(ctx, "stop", "clashcli.service"); e != nil {
				return errors.Join(err, e)
			}
			if e := m.Recover(ctx, true); e != nil {
				return e
			}
			if e := startService(ctx, p); e != nil {
				return e
			}
		}
		s, err = loadSettings(p)
		if err != nil {
			return err
		}
		if !(RealCore{p, s}).Running(ctx) {
			// Clear stale system proxy state under its lock before restarting an
			// unresponsive core; a failed start must not leave a dead proxy behind.
			if err = stopService(ctx, p); err != nil {
				return err
			}
			if err = startService(ctx, p); err != nil {
				return err
			}
		}
		if err = changeProxy(ctx, p, s.Proxy); err != nil {
			return err
		}
	}
	failures := []error{}
	for _, name := range []string{"systemctl", "ip"} {
		_, e := exec.LookPath(name)
		fmt.Printf("%s: %t\n", name, e == nil)
		if e != nil {
			failures = append(failures, e)
		}
	}
	for _, path := range []string{"/run/systemd/system", p.Core(), filepath.Join(p.Current(), "config.yaml"), filepath.Join(p.Data, "ui/current/index.html")} {
		_, e := os.Stat(path)
		fmt.Printf("%s: %t\n", path, e == nil)
		if e != nil {
			failures = append(failures, e)
		}
	}
	if _, e := os.Stat("/dev/net/tun"); e != nil {
		fmt.Println("/dev/net/tun: 不可用（仅开启 TUN 时需要）")
		if s.Tun {
			failures = append(failures, e)
		}
	} else {
		fmt.Println("/dev/net/tun: true")
	}
	if e := checkHealth(ctx, p, s); e != nil {
		failures = append(failures, e)
	}
	if _, e := os.Stat(filepath.Join(p.Data, "transaction.json")); e == nil {
		failures = append(failures, errors.New("存在未完成事务"))
	}
	if e := printStatus(ctx, p, false); e != nil {
		failures = append(failures, e)
	}
	return errors.Join(failures...)
}

// A status report is informative even when stopped; doctor is a health check
// with a meaningful exit code for automation.
func checkHealth(ctx context.Context, p Paths, s Settings) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var g Generation
	if err := readJSON(filepath.Join(p.Current(), "generation.json"), &g); err != nil {
		return fmt.Errorf("活动版本记录不可读: %w", err)
	}
	if err := (RealCore{p, s}).checkOnce(ctx, g); err != nil {
		return fmt.Errorf("内核未运行或实际配置不健康: %w", err)
	}
	actual, err := proxyActual(ctx, p)
	if err != nil {
		return err
	}
	if actual["managed"] != s.Proxy || s.Proxy && (actual["environment"] != true || actual["login_script"] != true || actual["desktop"] == false) {
		return errors.New("系统代理实际状态与保存偏好不一致；请运行 clashcli doctor --repair")
	}
	return nil
}
func rollbackPrevious(ctx context.Context, p Paths) error {
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	current, _ := os.Readlink(p.Current())
	content := generationContent(current)
	entries, err := os.ReadDir(filepath.Join(p.Data, "generations"))
	if err != nil {
		return err
	}
	var selected Generation
	var newest time.Time
	for _, e := range entries {
		path := filepath.Join(p.Data, "generations", e.Name())
		if path == current || generationContent(path) == content {
			continue
		}
		var g Generation
		if readJSON(filepath.Join(path, "generation.json"), &g) != nil || g.SourceID != s.Active || !g.Committed {
			continue
		}
		info, err := e.Info()
		if err == nil && info.ModTime().After(newest) {
			selected = g
			newest = info.ModTime()
		}
	}
	if selected.Path == "" {
		return errors.New("没有前一个保留版本")
	}
	g, err := cloneGeneration(p, s, selected.Path)
	if err != nil {
		return err
	}
	defer discardUncommitted(p, g.Path)
	core := RealCore{p, s}
	if err = core.Validate(ctx, &g); err != nil {
		return err
	}
	sub, err := s.Sub(s.Active)
	if err != nil {
		return err
	}
	sub.Generation = g.Path
	sub.ETag = ""
	sub.Modified = ""
	err = (Manager{P: p, Core: core}).Apply(ctx, g, s, s.Revision)
	if err == nil {
		pruneGenerations(p)
	}
	return err
}
func menu(ctx context.Context, p Paths) error {
	if !interactive() {
		return printStatus(ctx, p, false)
	}
	for {
		fmt.Println("\nclashcli\n1. 状态\n2. 系统代理开关\n3. TUN 开关\n4. 更新订阅\n5. 切换订阅\n6. 选择节点\n7. 查看日志\n8. UI 地址\n9. 启动 / 停止\n10. 管理订阅与更新周期\n0. 退出")
		choice, err := prompt("请选择", "1", false)
		if err != nil {
			return err
		}
		s, err := loadSettings(p)
		if err != nil {
			return err
		}
		switch choice {
		case "0":
			return nil
		case "1":
			err = printStatus(ctx, p, false)
		case "2":
			err = changeProxy(ctx, p, !s.Proxy)
		case "3":
			err = changeTun(ctx, p, !s.Tun)
		case "4":
			err = updateSubscription(ctx, p, "", false)
		case "5":
			names := []string{}
			for _, v := range s.Subscriptions {
				names = append(names, v.Name)
			}
			var name string
			name, err = choose("订阅", names)
			if err == nil {
				err = useSubscription(ctx, p, name)
			}
		case "6":
			err = selectNode(ctx, p, nil)
		case "7":
			err = showLogs(ctx, false, "", 50)
		case "8":
			fmt.Printf("http://127.0.0.1:%d/ui/\n密钥：clashcli ui --show-secret\n", s.ControllerPort)
		case "9":
			if (RealCore{p, s}).Running(ctx) {
				err = stopService(ctx, p)
			} else {
				err = startService(ctx, p)
			}
		case "10":
			var action string
			action, err = choose("订阅操作：添加 / 修改链接 / 更新周期 / 删除", []string{"add", "edit", "schedule", "remove"})
			if err == nil {
				args := []string{action}
				if action != "add" {
					names := []string{}
					for _, sub := range s.Subscriptions {
						names = append(names, sub.Name)
					}
					var name string
					name, err = choose("选择订阅", names)
					args = append(args, name)
				}
				if err == nil {
					command := subscriptionCommand(p)
					command.SilenceErrors, command.SilenceUsage = true, true
					command.SetArgs(args)
					err = command.ExecuteContext(ctx)
				}
			}
		default:
			err = errors.New("无效选项")
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, Redact(err.Error()))
		}
	}
}
