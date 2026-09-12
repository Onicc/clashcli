package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.yaml.in/yaml/v3"
)

type Transaction struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	OldSettings []byte `json:"old_settings"`
	NewSettings []byte `json:"new_settings"`
	Phase       string `json:"phase"`
}
type Manager struct {
	P    Paths
	Core Core
	Hook func(string) error
}

func (m Manager) journal() string { return filepath.Join(m.P.Data, "transaction.json") }
func (m Manager) checkpoint(stage string) error {
	if m.Hook != nil {
		return m.Hook(stage)
	}
	return nil
}
func (m Manager) Apply(ctx context.Context, g Generation, next Settings, revision uint64) error {
	err := m.applyLocked(ctx, g, next, revision)
	var recovery *rollbackFailure
	if !errors.As(err, &recovery) {
		return err
	}
	// Restart only after releasing the mutation lock: ExecStartPre also recovers transactions.
	repairCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if e := m.Recover(repairCtx, true); e != nil {
		return err
	}
	if e := m.Core.Restart(repairCtx); e != nil {
		return fmt.Errorf("旧配置已恢复，但内核重启失败；请运行 doctor --repair: %w", e)
	}
	s, e := loadSettings(m.P)
	if e != nil {
		return e
	}
	core := m.Core
	if real, ok := core.(RealCore); ok {
		real.S = s
		core = real
	}
	var previous Generation
	if e = readJSON(filepath.Join(m.P.Current(), "generation.json"), &previous); e == nil {
		e = core.Check(repairCtx, previous)
	}
	if e != nil {
		return fmt.Errorf("旧配置已恢复，但内核检查失败: %w", e)
	}
	return fmt.Errorf("更新失败，已恢复旧配置并重启内核: %w", recovery.cause)
}

type rollbackFailure struct{ cause, restore error }

func (e *rollbackFailure) Error() string {
	return fmt.Sprintf("更新失败（%v）；回滚未完成（%v），请运行 clashcli doctor --repair", e.cause, e.restore)
}
func (m Manager) applyLocked(ctx context.Context, g Generation, next Settings, revision uint64) error {
	unlock, err := lock(ctx, filepath.Join(m.P.Run, "apply.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if _, err = os.Stat(m.journal()); err == nil {
		return errors.New("存在未完成更新，请先运行 clashcli doctor --repair")
	}
	old, err := loadSettings(m.P)
	if err != nil {
		return err
	}
	if old.Revision != revision {
		return errors.New("配置已被其他命令修改，请重试；当前版本未替换")
	}
	oldPath, _ := os.Readlink(m.P.Current())
	next.Revision = old.Revision + 1
	oldYAML, err := yaml.Marshal(old)
	if err != nil {
		return err
	}
	newYAML, err := yaml.Marshal(next)
	if err != nil {
		return err
	}
	tx := Transaction{OldPath: oldPath, NewPath: g.Path, OldSettings: oldYAML, NewSettings: newYAML, Phase: "prepared"}
	if err = writeJSON(m.journal(), tx); err != nil {
		return err
	}
	if err = m.checkpoint("prepared"); err != nil {
		return err
	}
	wasRunning := m.Core.Running(ctx)
	if err = switchLink(m.P.Current(), g.Path); err != nil {
		return m.rollbackError(tx, wasRunning, err)
	}
	if err = m.checkpoint("switched"); err != nil {
		return err
	}
	if wasRunning {
		if err = m.Core.Reload(ctx, g.Path); err != nil {
			return m.rollbackError(tx, true, err)
		}
		if err = m.Core.Check(ctx, g); err != nil {
			return m.rollbackError(tx, true, err)
		}
	}
	if err = m.checkpoint("reloaded"); err != nil {
		return err
	}
	if err = atomicWrite(m.P.Settings(), newYAML, 0600); err != nil {
		return m.rollbackError(tx, wasRunning, err)
	}
	if err = m.checkpoint("settings"); err != nil {
		return err
	}
	tx.Phase = "committed"
	if err = writeJSON(m.journal(), tx); err != nil {
		return m.rollbackError(tx, wasRunning, err)
	}
	if err = m.checkpoint("committed"); err != nil {
		return err
	}
	if err = markCommitted(g.Path); err != nil {
		return err
	}
	if err = os.Remove(m.journal()); err != nil {
		return err
	}
	return syncDir(m.P.Data)
}
func (m Manager) rollbackError(tx Transaction, running bool, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := m.restore(ctx, tx, running); err != nil {
		return &rollbackFailure{cause, err}
	}
	return fmt.Errorf("更新失败，已恢复原配置: %w", cause)
}
func (m Manager) restore(ctx context.Context, tx Transaction, running bool) error {
	if tx.OldPath != "" {
		if !inside(filepath.Join(m.P.Data, "generations"), tx.OldPath) {
			return errors.New("事务旧版本路径无效")
		}
		if err := switchLink(m.P.Current(), tx.OldPath); err != nil {
			return err
		}
	} else {
		if err := os.Remove(m.P.Current()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := atomicWrite(m.P.Settings(), tx.OldSettings, 0600); err != nil {
		return err
	}
	if running && tx.OldPath != "" {
		var old Settings
		if err := yaml.Unmarshal(tx.OldSettings, &old); err != nil {
			return err
		}
		core := m.Core
		if real, ok := core.(RealCore); ok {
			real.S = old
			core = real
		}
		var generation Generation
		if err := readJSON(filepath.Join(tx.OldPath, "generation.json"), &generation); err != nil {
			return err
		}
		err := core.Reload(ctx, tx.OldPath)
		if err == nil {
			err = core.Check(ctx, generation)
		}
		if err != nil {
			return fmt.Errorf("旧版本恢复后内核检查失败: %w", err)
		}
	}
	if err := os.Remove(m.journal()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(m.P.Data)
}
func (m Manager) Recover(ctx context.Context, offline bool) error {
	if _, err := os.Stat(m.journal()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, err := lock(ctx, filepath.Join(m.P.Run, "apply.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	var tx Transaction
	if err = readJSON(m.journal(), &tx); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if tx.Phase == "committed" {
		if err = atomicWrite(m.P.Settings(), tx.NewSettings, 0600); err != nil {
			return err
		}
		if err = switchLink(m.P.Current(), tx.NewPath); err != nil {
			return err
		}
		if err = markCommitted(tx.NewPath); err != nil {
			return err
		}
		if !offline && m.Core.Running(ctx) {
			var next Settings
			var generation Generation
			if err = yaml.Unmarshal(tx.NewSettings, &next); err != nil {
				return err
			}
			if err = readJSON(filepath.Join(tx.NewPath, "generation.json"), &generation); err != nil {
				return err
			}
			core := m.Core
			if real, ok := core.(RealCore); ok {
				real.S = next
				core = real
			}
			if err = core.Reload(ctx, tx.NewPath); err != nil {
				return err
			}
			if err = core.Check(ctx, generation); err != nil {
				return err
			}
		}
		return os.Remove(m.journal())
	}
	return m.restore(ctx, tx, !offline && m.Core.Running(ctx))
}
func mutateSettings(ctx context.Context, p Paths, fn func(*Settings) error) error {
	unlock, err := lock(ctx, filepath.Join(p.Run, "apply.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if _, err = os.Stat(filepath.Join(p.Data, "transaction.json")); err == nil {
		return errors.New("存在未完成事务，请运行 doctor --repair")
	}
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	if err = fn(&s); err != nil {
		return err
	}
	s.Revision++
	return saveSettings(p, s)
}
func updateSubscription(ctx context.Context, p Paths, name string, force bool) error {
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	sub, err := s.Sub(name)
	if err != nil {
		return err
	}
	id := sub.ID
	unlock, err := lock(ctx, filepath.Join(p.Run, "sub-"+id+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	// Fetch with a subscription lock, but do not hold the global mutation lock over network IO.
	s, err = loadSettings(p)
	if err != nil {
		return err
	}
	sub, err = s.Sub(id)
	if err != nil {
		return err
	}
	etag, modified := sub.ETag, sub.Modified
	if force {
		etag = ""
		modified = ""
	}
	d, err := download(ctx, sub.URL, maxSubscription, etag, modified)
	if err != nil {
		recordUpdateError(p, id, err)
		return err
	}
	if d.Unchanged {
		if sub.Generation == "" {
			return errors.New("收到 304 但本地没有订阅版本，请使用 sub update --force")
		}
		// A 304 only covers the main document: referenced providers still need updating.
		d.Body, err = os.ReadFile(filepath.Join(sub.Generation, "source"))
		if err != nil {
			return errors.New("缓存订阅原文缺失，请使用 sub update --force")
		}
	}
	g, err := buildGeneration(ctx, p, s, *sub, d.Body)
	if err != nil {
		recordUpdateError(p, id, err)
		return err
	}
	keep := false
	defer func() {
		if !keep {
			if _, e := os.Stat(filepath.Join(p.Data, "transaction.json")); errors.Is(e, os.ErrNotExist) {
				_ = removeGeneration(p, g.Path)
			}
		}
	}()
	if sub.Generation != "" && sameGeneration(sub.Generation, g.Path) {
		return mutateSettings(ctx, p, func(current *Settings) error {
			if current.Revision != s.Revision {
				return errors.New("配置已改变，请重试")
			}
			v, e := current.Sub(id)
			if e != nil {
				return e
			}
			v.LastChecked = time.Now().UTC()
			v.LastError = ""
			v.ETag = d.ETag
			v.Modified = d.Modified
			return nil
		})
	}
	core := RealCore{p, s}
	if err = core.Validate(ctx, &g); err != nil {
		recordUpdateError(p, id, err)
		return err
	}
	sub.Generation = g.Path
	sub.ETag = d.ETag
	sub.Modified = d.Modified
	sub.LastChecked = time.Now().UTC()
	sub.LastUpdated = sub.LastChecked
	sub.LastError = ""
	if s.Active == id {
		err = (Manager{P: p, Core: core}).Apply(ctx, g, s, s.Revision)
	} else {
		err = mutateSettings(ctx, p, func(current *Settings) error {
			if current.Revision != s.Revision {
				return errors.New("配置在更新期间发生变化，请重试")
			}
			*current = s
			if err := markCommitted(g.Path); err != nil {
				return err
			}
			return nil
		})
	}
	if err != nil {
		recordUpdateError(p, id, err)
		return err
	}
	keep = true
	pruneGenerations(p)
	return nil
}
func recordUpdateError(p Paths, id string, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = mutateSettings(ctx, p, func(s *Settings) error {
		sub, err := s.Sub(id)
		if err != nil {
			return err
		}
		sub.LastChecked = time.Now().UTC()
		sub.LastError = Redact(cause.Error())
		return nil
	})
}
func useSubscription(ctx context.Context, p Paths, name string) error {
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	sub, err := s.Sub(name)
	if err != nil {
		return err
	}
	if sub.Generation == "" {
		if err = updateSubscription(ctx, p, sub.ID, true); err != nil {
			return err
		}
		s, err = loadSettings(p)
		if err != nil {
			return err
		}
		sub, err = s.Sub(name)
		if err != nil {
			return err
		}
	}
	s.Active = sub.ID
	g, err := cloneGeneration(p, s, sub.Generation)
	if err != nil {
		return err
	}
	defer discardUncommitted(p, g.Path)
	sub.Generation = g.Path
	core := RealCore{p, s}
	if err = core.Validate(ctx, &g); err != nil {
		_ = removeGeneration(p, g.Path)
		return err
	}
	err = (Manager{P: p, Core: core}).Apply(ctx, g, s, s.Revision)
	if err == nil {
		pruneGenerations(p)
	}
	return err
}
func changeTun(ctx context.Context, p Paths, enabled bool) error {
	s, err := loadSettings(p)
	if err != nil {
		return err
	}
	if enabled {
		if _, err = os.Stat("/dev/net/tun"); err != nil {
			return errors.New("/dev/net/tun 不可用；请启用 Linux TUN 设备")
		}
	}
	s.Tun = enabled
	from, err := os.Readlink(p.Current())
	if err != nil {
		return errors.New("尚未启用订阅")
	}
	g, err := cloneGeneration(p, s, from)
	if err != nil {
		return err
	}
	defer discardUncommitted(p, g.Path)
	core := RealCore{p, s}
	if err = core.Validate(ctx, &g); err != nil {
		_ = removeGeneration(p, g.Path)
		return err
	}
	if sub, err := s.Sub(s.Active); err == nil {
		sub.Generation = g.Path
	}
	err = (Manager{P: p, Core: core}).Apply(ctx, g, s, s.Revision)
	if err == nil {
		pruneGenerations(p)
	}
	return err
}
func pruneGenerations(p Paths) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := lock(ctx, filepath.Join(p.Run, "apply.lock"))
	if err != nil {
		return
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(p.Data, "transaction.json")); err == nil {
		return
	}
	s, err := loadSettings(p)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	sources := map[string]bool{}
	current, _ := os.Readlink(p.Current())
	keep[current] = true
	for _, sub := range s.Subscriptions {
		keep[sub.Generation] = true
		sources[sub.ID] = true
	}
	entries, err := os.ReadDir(filepath.Join(p.Data, "generations"))
	if err != nil {
		return
	}
	type item struct {
		path   string
		mtime  time.Time
		source string
	}
	items := []item{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(p.Data, "generations", e.Name())
		var g Generation
		if readJSON(filepath.Join(path, "generation.json"), &g) != nil {
			continue
		}
		if !g.Committed {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path, info.ModTime(), g.SourceID})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mtime.After(items[j].mtime) })
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, v := range items {
		key := v.source + ":" + generationContent(v.path)
		retain := false
		if !seen[key] {
			seen[key] = true
			counts[v.source]++
			retain = sources[v.source] && counts[v.source] <= 2
		}
		if retain || keep[v.path] {
			continue
		}
		_ = removeGeneration(p, v.path)
	}
}
func discardUncommitted(p Paths, path string) {
	if _, err := os.Stat(filepath.Join(p.Data, "transaction.json")); !errors.Is(err, os.ErrNotExist) {
		return
	}
	var g Generation
	if readJSON(filepath.Join(path, "generation.json"), &g) == nil && !g.Committed {
		_ = removeGeneration(p, path)
	}
}
