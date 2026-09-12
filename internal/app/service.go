package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func installService(ctx context.Context, p Paths) error {
	if err := os.MkdirAll(p.Units, 0755); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=clashcli mihomo proxy
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=3

[Service]
Type=simple
User=root
UMask=0077
RuntimeDirectory=clashcli
RuntimeDirectoryMode=0700
RuntimeDirectoryPreserve=yes
ExecStartPre=+%s internal recover
ExecStart=%s -d %s -f %s/config.yaml
ExecStartPost=+%s internal proxy-restore
ExecStopPost=+%s internal proxy-clear
Restart=on-failure
RestartSec=3
TimeoutStartSec=90
TimeoutStopSec=30
KillMode=control-group
NoNewPrivileges=true
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE CAP_DAC_READ_SEARCH
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s %s
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
Environment=SAFE_PATHS=%s
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`, p.Executable(), p.Core(), p.Data, p.Current(), p.Executable(), p.Executable(), p.Data, p.Run, p.Data)
	if err := atomicWrite(filepath.Join(p.Units, "clashcli.service"), []byte(unit), 0644); err != nil {
		return err
	}
	update := fmt.Sprintf(`[Unit]
Description=clashcli subscription update %%i
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
UMask=0077
ExecStart=%s sub update --id %%i
TimeoutStartSec=10min
Nice=10
`, p.Executable())
	if err := atomicWrite(filepath.Join(p.Units, "clashcli-update@.service"), []byte(update), 0644); err != nil {
		return err
	}
	return systemctl(ctx, "daemon-reload")
}

var subscriptionID = regexp.MustCompile(`^[a-f0-9]{16}$`)

func timerName(id string) (string, error) {
	if !subscriptionID.MatchString(id) {
		return "", errors.New("订阅 ID 无效")
	}
	return "clashcli-update@" + id + ".timer", nil
}
func configureTimer(ctx context.Context, p Paths, sub Subscription) error {
	name, err := timerName(sub.ID)
	if err != nil {
		return err
	}
	if sub.Calendar == "" {
		_ = systemctl(ctx, "disable", "--now", name)
		_ = systemctl(ctx, "clean", "--what=state", name)
		if err = os.Remove(filepath.Join(p.Units, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return systemctl(ctx, "daemon-reload")
	}
	if strings.ContainsAny(sub.Calendar, "\r\n\x00") {
		return errors.New("定时表达式不能包含换行")
	}
	if _, err = commandOutput(ctx, "systemd-analyze", "calendar", sub.Calendar); err != nil {
		return errors.New("无效的 systemd 日历表达式")
	}
	unit := fmt.Sprintf("[Unit]\nDescription=clashcli scheduled update\n\n[Timer]\nOnCalendar=%s\nPersistent=true\nAccuracySec=1s\nRandomizedDelaySec=60\nUnit=clashcli-update@%s.service\n\n[Install]\nWantedBy=timers.target\n", sub.Calendar, sub.ID)
	if err = atomicWrite(filepath.Join(p.Units, name), []byte(unit), 0644); err != nil {
		return err
	}
	if err = systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return systemctl(ctx, "enable", "--now", name)
}
func startService(ctx context.Context, p Paths) error {
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(p.Current(), "config.yaml")); err != nil {
		return errors.New("请先添加并启用订阅")
	}
	if err = systemctl(ctx, "start", "clashcli.service"); err != nil {
		return err
	}
	var g Generation
	if err = readJSON(filepath.Join(p.Current(), "generation.json"), &g); err != nil {
		return err
	}
	return (RealCore{p, s}).Check(ctx, g)
}
func stopService(ctx context.Context, p Paths) error {
	unlock, err := lock(ctx, filepath.Join(p.Run, "proxy.lock"))
	if err != nil {
		return err
	}
	err = disableProxy(ctx, p)
	unlock() // ExecStopPost acquires the same lock.
	if err != nil {
		return err
	}
	return systemctl(ctx, "stop", "clashcli.service")
}
func removeInstallation(ctx context.Context, p Paths, purge bool) error {
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	for _, sub := range s.Subscriptions {
		sub.Calendar = ""
		if err = configureTimer(ctx, p, sub); err != nil {
			return err
		}
		if subscriptionID.MatchString(sub.ID) {
			if err = systemctl(ctx, "stop", "clashcli-update@"+sub.ID+".service"); err != nil {
				return err
			}
		}
	}
	if err = stopService(ctx, p); err != nil {
		return err
	}
	if err = systemctl(ctx, "disable", "clashcli.service"); err != nil {
		return err
	}
	for _, name := range []string{"clashcli.service", "clashcli-update@.service"} {
		if err = os.Remove(filepath.Join(p.Units, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err = systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	for _, path := range []string{p.Bin, p.Run} {
		if err = os.RemoveAll(path); err != nil {
			return err
		}
	}
	if purge {
		for _, path := range []string{p.Config, p.Data} {
			if err = os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	if err = os.Remove(p.Executable()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
