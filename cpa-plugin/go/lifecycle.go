package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	phaseConfigured = "configured"
	phaseReady      = "ready"
	phaseDegraded   = "degraded"
	phaseQuiescing  = "quiescing"
	phaseQuiesced   = "quiesced"
)

const quiesceWaitTimeout = 15 * time.Second

type lifecycleState struct {
	mu           sync.Mutex
	generation   uint64
	phase        string
	yamlError    string
	pathWarning  string
	workerCancel context.CancelFunc
	store        *stateStore
	cfg          pluginConfig
	lastValidCfg pluginConfig
	hasLastValid bool
	selectedPath string
}

var (
	life     lifecycleState
	workerWG sync.WaitGroup
	phaseVal atomic.Value // string
)

func init() {
	phaseVal.Store("")
}

func setPhase(phase string) {
	life.phase = phase
	phaseVal.Store(phase)
}

func currentPhase() string {
	v, _ := phaseVal.Load().(string)
	return v
}

func acceptsBackgroundWork() bool {
	switch currentPhase() {
	case phaseQuiescing, phaseQuiesced:
		return false
	default:
		return true
	}
}

func goTracked(fn func()) {
	if fn == nil {
		return
	}
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		fn()
	}()
}

func configure(raw []byte) error {
	return configureLifecycle(raw, false)
}

func configureLifecycle(raw []byte, reconfigure bool) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}

	life.mu.Lock()
	defer life.mu.Unlock()

	prev := life.cfg
	hasPrev := life.hasLastValid
	cfg := pluginConfig{}
	yamlErr := ""
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			yamlErr = err.Error()
			if hasPrev {
				cfg = prev
			} else {
				cfg = pluginConfig{}
			}
		}
	} else if hasPrev {
		cfg = prev
	}

	explicitPath := strings.TrimSpace(cfg.StateFile)
	if explicitPath == "" {
		if hasPrev && strings.TrimSpace(prev.StateFile) != "" {
			cfg.StateFile = prev.StateFile
		} else if yamlErr == "" {
			cfg.StateFile = resolveDefaultStateFile()
			life.selectedPath = cfg.StateFile
		}
	} else {
		life.selectedPath = explicitPath
	}
	if cfg.RotationTimeoutSec <= 0 {
		cfg.RotationTimeoutSec = 45
	}

	life.yamlError = yamlErr
	life.pathWarning = ""
	if yamlErr != "" && !hasPrev && strings.TrimSpace(cfg.StateFile) == "" {
		life.pathWarning = "YAML 无效且没有上一份有效配置，已拒绝自动改用默认状态路径"
		currentConfig.Store(cfg)
		life.cfg = cfg
		life.store = nil
		store = nil
		setPhase(phaseDegraded)
		return nil
	}
	if pathUsesTempDir(cfg.StateFile) {
		life.pathWarning = "状态文件位于临时目录，不能作为正式部署路径"
	}
	if explicitPath != "" && !dirWritable(filepath.Dir(explicitPath)) {
		life.pathWarning = "指定的状态目录不可写，已拒绝改用其他空目录"
	}

	sameIdentity := reconfigure && hasPrev &&
		strings.TrimSpace(prev.StateFile) == strings.TrimSpace(cfg.StateFile) &&
		prev.RotationURL == cfg.RotationURL &&
		prev.RotationTokenEnv == cfg.RotationTokenEnv &&
		sameStringSet(prev.RotatableNodeIDs, cfg.RotatableNodeIDs) &&
		life.store != nil &&
		currentPhase() != phaseQuiesced &&
		currentPhase() != phaseQuiescing

	currentConfig.Store(cfg)
	life.cfg = cfg
	if yamlErr == "" {
		life.lastValidCfg = cfg
		life.hasLastValid = true
	}

	if sameIdentity {
		store = life.store
		if life.store != nil && (life.store.loadStatus != loadStatusOK && life.store.loadStatus != loadStatusFresh) {
			setPhase(phaseDegraded)
		} else if yamlErr != "" || life.pathWarning != "" {
			setPhase(phaseDegraded)
		} else {
			setPhase(phaseReady)
		}
		return nil
	}

	allowCreate := true
	if reconfigure && hasPrev && strings.TrimSpace(prev.StateFile) == strings.TrimSpace(cfg.StateFile) &&
		life.store != nil && (life.store.loadStatus == loadStatusOK || life.store.loadStatus == loadStatusFresh) {
		allowCreate = false
	}

	cancel := life.workerCancel
	life.workerCancel = nil
	workerCancel = nil
	life.mu.Unlock()
	drained := true
	if cancel != nil {
		cancel()
		drained = waitWorkers(quiesceWaitTimeout)
	}
	life.mu.Lock()

	opened, loadErr := openStateStore(cfg.StateFile, openStoreOptions{AllowEmptyCreate: allowCreate})
	life.store = opened
	store = opened

	setPhase(phaseConfigured)
	if !drained {
		life.pathWarning = "旧后台任务未能及时停写，已拒绝启动新 worker，请受控重启 CPA"
		setPhase(phaseDegraded)
	}
	if loadErr != nil || yamlErr != "" || life.pathWarning != "" || (opened != nil && opened.loadStatus != loadStatusOK && opened.loadStatus != loadStatusFresh) {
		setPhase(phaseDegraded)
	}

	canStartWorker := drained && opened != nil && !opened.persistBlocked &&
		currentPhase() != phaseQuiescing && currentPhase() != phaseQuiesced
	if canStartWorker {
		ctx, workerStop := context.WithCancel(context.Background())
		life.workerCancel = workerStop
		workerCancel = workerStop
		life.generation++
		startGuardWorker(ctx, opened)
		if currentPhase() != phaseDegraded {
			setPhase(phaseReady)
		}
	}
	return nil
}

func waitWorkers(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func resetLifecycleForTest() {
	life.mu.Lock()
	cancel := life.workerCancel
	life.workerCancel = nil
	workerCancel = nil
	life.generation = 0
	life.phase = ""
	life.yamlError = ""
	life.pathWarning = ""
	life.store = nil
	life.cfg = pluginConfig{}
	life.lastValidCfg = pluginConfig{}
	life.hasLastValid = false
	life.selectedPath = ""
	life.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	waitWorkers(2 * time.Second)
	phaseVal.Store("")
}

func quiescePlugin() ([]byte, error) {
	life.mu.Lock()
	phase := currentPhase()
	if phase == phaseQuiesced {
		life.mu.Unlock()
		return okEnvelope(map[string]any{"quiesced": true, "phase": phaseQuiesced})
	}
	setPhase(phaseQuiescing)
	cancel := life.workerCancel
	life.workerCancel = nil
	workerCancel = nil
	st := life.store
	life.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(quiesceWaitTimeout):
		return errorEnvelope("restart_required", "无法在超时内停止后台写任务，请受控重启 CPA 后再升级"), nil
	}
	if st != nil {
		if err := st.Flush(); err != nil {
			return errorEnvelope("restart_required", "停写后刷盘失败: "+err.Error()), nil
		}
	}
	life.mu.Lock()
	setPhase(phaseQuiesced)
	life.mu.Unlock()
	return okEnvelope(map[string]any{"quiesced": true, "phase": phaseQuiesced})
}

func shutdownPlugin() {
	_, _ = quiescePlugin()
}

func dirWritable(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
		if seen[v] < 0 {
			return false
		}
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func lifecycleStatus() map[string]any {
	life.mu.Lock()
	defer life.mu.Unlock()
	path := ""
	if life.store != nil {
		path = life.store.path
	} else {
		path = life.cfg.StateFile
	}
	return map[string]any{
		"phase":          currentPhase(),
		"generation":     life.generation,
		"yaml_error":     life.yamlError,
		"path_warning":   life.pathWarning,
		"state_file":     path,
		"using_temp_dir": pathUsesTempDir(path),
		"method_quiesce": methodPluginQuiesce,
	}
}
