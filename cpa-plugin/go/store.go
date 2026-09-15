package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	stateSchemaVersion = 2
	maxStateSnapshots  = 3

	loadStatusOK           = "ok"
	loadStatusFresh        = "fresh"
	loadStatusMissing      = "missing"
	loadStatusPermission   = "permission"
	loadStatusCorrupt      = "corrupt"
	loadStatusIncompatible = "incompatible"

	nodeOriginManual    = "manual"
	nodeOriginDiscovery = "credential_discovery"
	nodeModeObserve     = "observe"
	nodeModeManage      = "manage"
)

type openStoreOptions struct {
	AllowEmptyCreate bool
}

type policyConfig struct {
	Mode              string  `json:"mode"`
	ActiveIntervalSec int     `json:"active_interval_seconds"`
	PassivePollSec    int     `json:"passive_poll_seconds"`
	QuarantineSec     int     `json:"quarantine_seconds"`
	SoftTPS           float64 `json:"soft_tps"`
	HardTPS           float64 `json:"hard_tps"`
	ConsecutiveSoft   int     `json:"consecutive_soft"`
	ConsecutiveErrors int     `json:"consecutive_errors"`
	MinHealthyNodes   int     `json:"min_healthy_nodes"`
	MinGenerationMs   int64   `json:"min_generation_ms"`
	MinOutputTokens   int64   `json:"min_output_tokens"`
	Model             string  `json:"model"`
	DisableAuthOnHard bool    `json:"disable_auth_on_hard"`
	// ThinkingGuard enables missing-thinking 降智 detection. When false, quality
	// classification falls back to the original soft/hard Token/s thresholds only.
	// Absent in old state.json → default true (see normalizePolicy).
	ThinkingGuard bool `json:"thinking_guard"`
	// ConsecutiveMissingThinking is how many consecutive missing-thinking samples
	// on the same egress are required before isolation / cross-verify. Default 1.
	ConsecutiveMissingThinking int `json:"consecutive_missing_thinking"`
	// ThinkingCrossVerify defers missing-thinking isolation until an active probe
	// also lacks thinking. Default false to minimize active probes; operators can
	// re-enable confirmation in the panel.
	ThinkingCrossVerify bool `json:"thinking_cross_verify"`
	// SoftCrossVerify defers soft-TPS isolation until an active probe confirms the
	// anomaly. Default false: passive observations quarantine directly to minimize
	// active probes; operators can re-enable confirmation in the panel.
	SoftCrossVerify      bool `json:"soft_cross_verify"`
	MaxOutputTokensProbe int  `json:"max_output_tokens"`
	// ActiveProfileID selects the built-in or custom probe recipe used by
	// automatic and manual quality tests. Empty falls back to throughput.
	ActiveProfileID string `json:"active_profile_id,omitempty"`
	// PolicySchema tracks policy feature revisions. Bump when new defaults must be
	// force-migrated once for existing state.json files.
	PolicySchema int `json:"policy_schema,omitempty"`
}

type nodeRecord struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	ProxyURL             string    `json:"-"` // never serialize to API clients in clear form via dedicated DTO
	ProxyURLStored       string    `json:"proxy_url"`
	Enabled              bool      `json:"enabled"`
	ProxyPool            bool      `json:"proxy_pool"`
	AccountCapacity      int       `json:"account_capacity"`
	Origin               string    `json:"origin,omitempty"`
	ManagementMode       string    `json:"management_mode,omitempty"`
	ExitIP               string    `json:"exit_ip,omitempty"`
	ProbeStatus          string    `json:"probe_status,omitempty"`
	ProbeLatencyMs       int64     `json:"probe_latency_ms,omitempty"`
	AssignedAccountCount int       `json:"assigned_account_count"`
	DisabledByGuard      bool      `json:"disabled_by_guard"`
	QuarantinedUntil     float64   `json:"quarantined_until,omitempty"`
	ErrorStrikes         int       `json:"error_strikes"`
	SoftStrikes          int       `json:"soft_strikes"`
	ThinkingStrikes      int       `json:"thinking_strikes"`
	LastClassification   string    `json:"last_classification,omitempty"`
	LastOutputTPS        float64   `json:"last_output_tps,omitempty"`
	LastFirstTokenMs     int64     `json:"last_first_token_ms,omitempty"`
	LastDurationMs       int64     `json:"last_duration_ms,omitempty"`
	LastOutputTokens     int64     `json:"last_output_tokens,omitempty"`
	LastReason           string    `json:"last_reason,omitempty"`
	LastSource           string    `json:"last_source,omitempty"`
	LastObservedAt       float64   `json:"last_observed_at,omitempty"`
	LastProbeAt          float64   `json:"last_probe_at,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type nodeCreateInput struct {
	Name            string
	ProxyURL        string
	Enabled         bool
	ProxyPool       bool
	AccountCapacity int
	Origin          string
	ManagementMode  string
}

type guardEvent struct {
	TS             float64 `json:"ts"`
	Event          string  `json:"event"`
	NodeID         string  `json:"node_id,omitempty"`
	NodeName       string  `json:"node_name,omitempty"`
	AuthID         string  `json:"auth_id,omitempty"`
	Reason         string  `json:"reason,omitempty"`
	Classification string  `json:"classification,omitempty"`
	OutputTPS      float64 `json:"output_tps,omitempty"`
}

type probeStats struct {
	Total        int64 `json:"total"`
	Healthy      int64 `json:"healthy"`
	Soft         int64 `json:"soft"`
	Hard         int64 `json:"hard"`
	Errors       int64 `json:"errors"`
	Ignored      int64 `json:"ignored"`
	OutputTokens int64 `json:"output_tokens"`
}

type actionStats struct {
	Quarantined int64 `json:"quarantined"`
	Restored    int64 `json:"restored"`
	Suppressed  int64 `json:"suppressed"`
}

type statistics struct {
	StartedAt float64     `json:"started_at"`
	Active    probeStats  `json:"active"`
	Passive   probeStats  `json:"passive"`
	Actions   actionStats `json:"actions"`
}

// authDegradeRecord tracks per-account 降智 (missing-thinking) hits.
type authDegradeRecord struct {
	AuthID        string  `json:"auth_id"`
	Label         string  `json:"label,omitempty"`
	DegradedCount int64   `json:"degraded_count"`
	SampleCount   int64   `json:"sample_count"`
	LastAt        float64 `json:"last_at,omitempty"`
	LastReason    string  `json:"last_reason,omitempty"`
	LastNodeID    string  `json:"last_node_id,omitempty"`
	LastNodeName  string  `json:"last_node_name,omitempty"`
	LastOutputTPS float64 `json:"last_output_tps,omitempty"`
	LastSource    string  `json:"last_source,omitempty"`
}

type guardState struct {
	Version        int                           `json:"version"`
	SchemaVersion  int                           `json:"schema_version,omitempty"`
	ConfigRevision int64                         `json:"config_revision,omitempty"`
	LastPersistAt  float64                       `json:"last_persist_at,omitempty"`
	Policy         policyConfig                  `json:"policy"`
	Nodes          map[string]*nodeRecord        `json:"nodes"`
	Profiles       map[string]*ProbeProfile      `json:"profiles"`
	Events         []guardEvent                  `json:"events"`
	Stats          statistics                    `json:"statistics"`
	AuthStats      map[string]*authDegradeRecord `json:"auth_stats"`
	NextID         int                           `json:"next_id"`
	UpdatedAt      float64                       `json:"updated_at"`
}

type stateStore struct {
	mu             sync.Mutex
	path           string
	data           guardState
	dirty          bool
	flushTimer     *time.Timer
	flushDelay     time.Duration
	loadStatus     string
	loadError      string
	lastPersistErr string
	persistBlocked bool
	writable       bool
}

func defaultPolicy() policyConfig {
	return policyConfig{
		Mode:                       "hybrid",
		ActiveIntervalSec:          1800,
		PassivePollSec:             5,
		QuarantineSec:              3600,
		SoftTPS:                    500,
		HardTPS:                    1000,
		ConsecutiveSoft:            2,
		ConsecutiveErrors:          2,
		MinHealthyNodes:            1,
		MinGenerationMs:            1000,
		MinOutputTokens:            32,
		Model:                      "grok-4.5",
		DisableAuthOnHard:          true,
		ThinkingGuard:              true,
		ConsecutiveMissingThinking: 1,
		ThinkingCrossVerify:        false,
		SoftCrossVerify:            false,
		MaxOutputTokensProbe:       384,
		ActiveProfileID:            defaultProbeProfileID(),
		PolicySchema:               4,
	}
}

// normalizePolicy fills zero / missing fields with defaults.
// Bool feature flags that default to true cannot be distinguished from explicit
// false after a plain Unmarshal; load paths pass the raw policy object so absent
// keys become defaults instead of false.
func normalizePolicy(p *policyConfig, rawPolicy map[string]any) {
	if p == nil {
		return
	}
	def := defaultPolicy()
	if p.Mode == "" {
		p.Mode = def.Mode
	}
	if p.ActiveIntervalSec <= 0 {
		p.ActiveIntervalSec = def.ActiveIntervalSec
	}
	if p.PassivePollSec <= 0 {
		p.PassivePollSec = def.PassivePollSec
	}
	if p.QuarantineSec <= 0 {
		p.QuarantineSec = def.QuarantineSec
	}
	if p.SoftTPS <= 0 {
		p.SoftTPS = def.SoftTPS
	}
	if p.HardTPS <= 0 {
		p.HardTPS = def.HardTPS
	}
	if p.ConsecutiveSoft <= 0 {
		p.ConsecutiveSoft = def.ConsecutiveSoft
	}
	if p.ConsecutiveErrors <= 0 {
		p.ConsecutiveErrors = def.ConsecutiveErrors
	}
	if p.MinHealthyNodes <= 0 {
		p.MinHealthyNodes = def.MinHealthyNodes
	}
	if p.MinGenerationMs <= 0 {
		p.MinGenerationMs = def.MinGenerationMs
	}
	if p.MinOutputTokens <= 0 {
		p.MinOutputTokens = def.MinOutputTokens
	}
	if p.Model == "" {
		p.Model = def.Model
	}
	if p.MaxOutputTokensProbe <= 0 {
		p.MaxOutputTokensProbe = def.MaxOutputTokensProbe
	}
	if strings.TrimSpace(p.ActiveProfileID) == "" {
		p.ActiveProfileID = defaultProbeProfileID()
	}
	if p.ConsecutiveMissingThinking <= 0 {
		p.ConsecutiveMissingThinking = def.ConsecutiveMissingThinking
	}

	// Detect whether bool keys were explicitly present in the raw JSON object.
	has := func(keys ...string) bool {
		if rawPolicy == nil {
			return false
		}
		for _, k := range keys {
			if _, ok := rawPolicy[k]; ok {
				return true
			}
		}
		return false
	}
	if !has("thinking_guard", "thinkingGuard") {
		p.ThinkingGuard = def.ThinkingGuard
	}
	if !has("disable_auth_on_hard", "disableAuthOnHard") {
		p.DisableAuthOnHard = def.DisableAuthOnHard
	}
	if !has("thinking_cross_verify", "thinkingCrossVerify") {
		p.ThinkingCrossVerify = def.ThinkingCrossVerify
	}
	if !has("soft_cross_verify", "softCrossVerify") {
		p.SoftCrossVerify = def.SoftCrossVerify
	}

	// Schema bump records the product revision. Explicit operator values stay.
	// Absent keys already received the current defaults above; do not overwrite
	// a present true/false just because the packaged default changed.
	if p.PolicySchema < def.PolicySchema {
		p.PolicySchema = def.PolicySchema
	}

	if !p.ThinkingGuard {
		p.ThinkingCrossVerify = false
	}
}

func emptyGuardState() guardState {
	data := guardState{
		Version:       1,
		SchemaVersion: stateSchemaVersion,
		Policy:        defaultPolicy(),
		Nodes:         map[string]*nodeRecord{},
		Profiles:      map[string]*ProbeProfile{},
		Events:        nil,
		Stats:         statistics{StartedAt: float64(time.Now().Unix())},
		NextID:        1,
	}
	seedBuiltinProfiles(&data)
	return data
}

func newStateStore(path string) *stateStore {
	s, _ := openStateStore(path, openStoreOptions{AllowEmptyCreate: true})
	return s
}

func openStateStore(path string, opts openStoreOptions) (*stateStore, error) {
	s := &stateStore{path: path, flushDelay: 2 * time.Second, data: emptyGuardState()}
	err := s.load(opts)
	return s, err
}

func (s *stateStore) load(opts openStoreOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(opts)
}

func (s *stateStore) loadLocked(opts openStoreOptions) error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			if len(listSnapshotPaths(s.path)) > 0 || hasCorruptBackup(s.path) {
				s.markLoadFailure(loadStatusMissing, "状态文件缺失，但发现备份或损坏副本；已阻止空状态覆盖")
				return fmt.Errorf("%s", s.loadError)
			}
			if !opts.AllowEmptyCreate {
				s.markLoadFailure(loadStatusMissing, "状态文件不存在；已阻止当作全新安装覆盖")
				return err
			}
			s.loadStatus = loadStatusFresh
			s.loadError = ""
			s.persistBlocked = false
			if perr := s.writeStateLocked(false); perr != nil {
				s.writable = false
				s.lastPersistErr = perr.Error()
				s.dirty = true
				return perr
			}
			s.loadStatus = loadStatusOK
			return nil
		}
		if os.IsPermission(err) {
			s.writable = false
			s.markLoadFailure(loadStatusPermission, "状态文件不可读：权限不足")
			return err
		}
		s.markLoadFailure(loadStatusPermission, "读取状态文件失败: "+err.Error())
		return err
	}
	var data guardState
	if err := json.Unmarshal(raw, &data); err != nil {
		preserveCorruptFile(s.path, raw)
		s.markLoadFailure(loadStatusCorrupt, "状态文件 JSON 损坏，已保留原件并阻止覆盖")
		return err
	}
	fileSchema := data.SchemaVersion
	if fileSchema == 0 {
		fileSchema = data.Version
	}
	if fileSchema > stateSchemaVersion {
		s.markLoadFailure(loadStatusIncompatible, fmt.Sprintf("状态 schema %d 新于当前插件 %d，已阻止加载", fileSchema, stateSchemaVersion))
		return fmt.Errorf("%s", s.loadError)
	}
	if data.Nodes == nil {
		data.Nodes = map[string]*nodeRecord{}
	}
	if data.AuthStats == nil {
		data.AuthStats = map[string]*authDegradeRecord{}
	}
	if data.Profiles == nil {
		data.Profiles = map[string]*ProbeProfile{}
	}
	if data.NextID <= 0 {
		data.NextID = 1
	}
	seedBuiltinProfiles(&data)
	var rawRoot map[string]any
	_ = json.Unmarshal(raw, &rawRoot)
	var rawPolicy map[string]any
	if rp, ok := rawRoot["policy"].(map[string]any); ok {
		rawPolicy = rp
	}
	if data.Policy.HardTPS <= 0 {
		data.Policy = defaultPolicy()
		rawPolicy = nil
	}
	beforeSchema := 0
	if rawPolicy != nil {
		if v, ok := rawPolicy["policy_schema"].(float64); ok {
			beforeSchema = int(v)
		}
	}
	normalizePolicy(&data.Policy, rawPolicy)
	for _, n := range data.Nodes {
		hydrateNode(n)
	}
	data.SchemaVersion = stateSchemaVersion
	s.data = data
	s.loadStatus = loadStatusOK
	s.loadError = ""
	s.persistBlocked = false
	if beforeSchema < defaultPolicy().PolicySchema {
		s.data.Events = append(s.data.Events, guardEvent{
			TS:    float64(time.Now().Unix()),
			Event: "policy_migrated",
			Reason: fmt.Sprintf(
				"策略 schema 从 %d 升级到 %d：缺省字段填入当前默认值，已存在的显式布尔/隔离间隔保持不变",
				beforeSchema, s.data.Policy.PolicySchema,
			),
		})
		if len(s.data.Events) > 100 {
			s.data.Events = s.data.Events[len(s.data.Events)-100:]
		}
		if err := s.writeStateLocked(true); err != nil {
			s.dirty = true
			s.lastPersistErr = err.Error()
			return nil
		}
	}
	return nil
}

func (s *stateStore) markLoadFailure(status, message string) {
	s.loadStatus = status
	s.loadError = message
	s.persistBlocked = true
	s.dirty = false
}

func hydrateNode(n *nodeRecord) {
	if n == nil {
		return
	}
	n.ProxyURL = n.ProxyURLStored
	if strings.TrimSpace(n.Origin) == "" {
		n.Origin = nodeOriginManual
	}
	if strings.TrimSpace(n.ManagementMode) == "" {
		n.ManagementMode = nodeModeManage
	}
}

func (s *stateStore) persistLocked() error {
	return s.writeStateLocked(true)
}

func (s *stateStore) writeStateLocked(snapshot bool) error {
	if s.persistBlocked {
		err := fmt.Errorf("状态持久化已阻止：%s", s.loadError)
		s.lastPersistErr = err.Error()
		s.dirty = true
		return err
	}
	if strings.TrimSpace(s.path) == "" {
		err := fmt.Errorf("状态路径未设置")
		s.lastPersistErr = err.Error()
		s.dirty = true
		return err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		last = s.writeStateOnceLocked(snapshot)
		if last == nil {
			s.dirty = false
			s.lastPersistErr = ""
			s.writable = true
			return nil
		}
	}
	s.dirty = true
	s.lastPersistErr = last.Error()
	s.writable = false
	return last
}

func (s *stateStore) writeStateOnceLocked(snapshot bool) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	now := float64(time.Now().Unix())
	s.data.UpdatedAt = now
	s.data.SchemaVersion = stateSchemaVersion
	if s.data.Version <= 0 {
		s.data.Version = 1
	}
	for _, n := range s.data.Nodes {
		n.ProxyURLStored = n.ProxyURL
	}
	raw, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpName, s.path); err != nil {
		return err
	}
	cleanup = false
	s.data.LastPersistAt = now
	_ = os.Chmod(s.path, 0o600)
	if snapshot {
		_ = snapshotCurrentState(s.path)
	}
	return nil
}

func snapshotCurrentState(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := rotateSnapshots(path); err != nil {
		return err
	}
	return copyFile(path, path+".snap-1")
}

func rotateSnapshots(path string) error {
	_ = os.Remove(path + fmt.Sprintf(".snap-%d", maxStateSnapshots))
	for i := maxStateSnapshots - 1; i >= 1; i-- {
		from := path + fmt.Sprintf(".snap-%d", i)
		to := path + fmt.Sprintf(".snap-%d", i+1)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		_ = os.Remove(to)
		if err := os.Rename(from, to); err != nil {
			if err := copyFile(from, to); err != nil {
				return err
			}
			_ = os.Remove(from)
		}
	}
	return nil
}

func listSnapshotPaths(path string) []string {
	out := make([]string, 0, maxStateSnapshots)
	for i := 1; i <= maxStateSnapshots; i++ {
		p := path + fmt.Sprintf(".snap-%d", i)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func hasCorruptBackup(path string) bool {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	prefix := base + ".corrupt-"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			return true
		}
	}
	return false
}

func preserveCorruptFile(path string, raw []byte) {
	stamp := time.Now().UTC().Format("20060102T150405")
	backup := path + ".corrupt-" + stamp
	if _, err := os.Stat(backup); err == nil {
		return
	}
	_ = os.WriteFile(backup, raw, 0o600)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func replaceFile(src, dst string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(src, dst)
	}
	bak := dst + ".replace-bak"
	_ = os.Remove(bak)
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, bak); err != nil {
			if err2 := os.Remove(dst); err2 != nil {
				return err
			}
		}
	}
	if err := os.Rename(src, dst); err != nil {
		if _, statErr := os.Stat(bak); statErr == nil {
			_ = os.Rename(bak, dst)
		}
		return err
	}
	_ = os.Remove(bak)
	return nil
}

func (s *stateStore) scheduleFlushLocked() {
	s.dirty = true
	delay := s.flushDelay
	if delay <= 0 {
		delay = 2 * time.Second
	}
	if s.flushTimer != nil {
		return
	}
	s.flushTimer = time.AfterFunc(delay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.flushTimer = nil
		if !s.dirty {
			return
		}
		_ = s.writeStateLocked(false)
	})
}

func (s *stateStore) flushNowLocked() error {
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	return s.writeStateLocked(true)
}

func (s *stateStore) health() map[string]any {
	if s == nil {
		return map[string]any{"load_status": "uninitialized"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"path":               s.path,
		"load_status":        s.loadStatus,
		"load_error":         s.loadError,
		"last_persist_error": s.lastPersistErr,
		"last_persist_at":    s.data.LastPersistAt,
		"config_revision":    s.data.ConfigRevision,
		"schema_version":     s.data.SchemaVersion,
		"persist_blocked":    s.persistBlocked,
		"writable":           s.writable,
		"dirty":              s.dirty,
		"using_temp_dir":     pathUsesTempDir(s.path),
		"snapshots":          len(listSnapshotPaths(s.path)),
	}
}

func (s *stateStore) restoreLatestSnapshot() error {
	if s == nil {
		return fmt.Errorf("状态存储未初始化")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 1; i <= maxStateSnapshots; i++ {
		snap := s.path + fmt.Sprintf(".snap-%d", i)
		raw, err := os.ReadFile(snap)
		if err != nil {
			continue
		}
		var data guardState
		if json.Unmarshal(raw, &data) != nil {
			continue
		}
		if err := copyFile(snap, s.path); err != nil {
			return err
		}
		s.persistBlocked = false
		s.loadError = ""
		return s.loadLocked(openStoreOptions{AllowEmptyCreate: false})
	}
	return fmt.Errorf("没有可用的状态快照")
}

func (s *stateStore) reinitializeEmpty() error {
	if s == nil {
		return fmt.Errorf("状态存储未初始化")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("状态路径未设置")
	}
	s.data = emptyGuardState()
	s.persistBlocked = false
	s.loadStatus = loadStatusFresh
	s.loadError = ""
	s.dirty = true
	if err := s.writeStateLocked(true); err != nil {
		return err
	}
	s.loadStatus = loadStatusOK
	return nil
}

func pathUsesTempDir(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	tmp, err := filepath.Abs(os.TempDir())
	if err != nil {
		return false
	}
	sep := string(os.PathSeparator)
	if !strings.HasSuffix(tmp, sep) {
		tmp += sep
	}
	return strings.HasPrefix(abs, tmp)
}

// Flush writes any dirty state. Safe for shutdown / tests.
func (s *stateStore) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty && s.flushTimer == nil {
		// still fine to no-op; callers may want a barrier after critical ops
		return nil
	}
	return s.flushNowLocked()
}

func (s *stateStore) snapshot() guardState {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.data)
	var out guardState
	_ = json.Unmarshal(raw, &out)
	if out.Nodes == nil {
		out.Nodes = map[string]*nodeRecord{}
	}
	for id, n := range s.data.Nodes {
		if out.Nodes[id] != nil {
			out.Nodes[id].ProxyURL = n.ProxyURL
		}
	}
	return out
}

func (s *stateStore) policy() policyConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Policy
}

func (s *stateStore) updatePolicy(p policyConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.SoftTPS <= 0 || p.HardTPS <= 0 || p.SoftTPS >= p.HardTPS {
		return fmt.Errorf("软阈值必须低于硬阈值且都大于 0")
	}
	if p.Mode == "" {
		p.Mode = "hybrid"
	}
	if p.Mode != "active" && p.Mode != "passive" && p.Mode != "hybrid" {
		return fmt.Errorf("模式必须是 active、passive 或 hybrid")
	}
	if p.Model == "" {
		p.Model = "grok-4.5"
	}
	if p.ConsecutiveSoft <= 0 {
		p.ConsecutiveSoft = 2
	}
	if p.ConsecutiveErrors <= 0 {
		p.ConsecutiveErrors = 2
	}
	if p.ConsecutiveMissingThinking <= 0 {
		p.ConsecutiveMissingThinking = 1
	}
	if p.ConsecutiveMissingThinking > 50 {
		return fmt.Errorf("连续缺 thinking 次数需在 1 到 50 之间")
	}
	if p.QuarantineSec <= 0 {
		p.QuarantineSec = 120
	}
	if p.ActiveIntervalSec < 60 || p.ActiveIntervalSec > 86400 {
		return fmt.Errorf("主动检测间隔需在 60 到 86400 秒之间")
	}
	if p.PassivePollSec < 1 || p.PassivePollSec > 3600 {
		return fmt.Errorf("被动审计间隔需在 1 到 3600 秒之间")
	}
	if p.QuarantineSec < 10 || p.QuarantineSec > 86400 {
		return fmt.Errorf("隔离复测间隔需在 10 到 86400 秒之间")
	}
	if p.MinHealthyNodes <= 0 {
		p.MinHealthyNodes = 1
	}
	if p.MinGenerationMs < 200 || p.MinGenerationMs > 10000 {
		return fmt.Errorf("最短生成窗口需在 200 到 10000 毫秒之间")
	}
	if p.MinOutputTokens < 1 || p.MinOutputTokens > 10000 {
		return fmt.Errorf("最小判定 Token 数需在 1 到 10000 之间")
	}
	if p.MaxOutputTokensProbe < 16 || p.MaxOutputTokensProbe > 4096 {
		return fmt.Errorf("主动探测最大输出需在 16 到 4096 Token 之间")
	}
	if !p.ThinkingGuard {
		p.ThinkingCrossVerify = false
	}
	if strings.TrimSpace(p.ActiveProfileID) == "" {
		p.ActiveProfileID = defaultProbeProfileID()
	}
	if _, ok := s.data.Profiles[p.ActiveProfileID]; !ok {
		return fmt.Errorf("探针方案 %s 不存在", p.ActiveProfileID)
	}
	previous := s.data.Policy
	previousRev := s.data.ConfigRevision
	s.data.Policy = p
	s.data.ConfigRevision++
	if err := s.persistLocked(); err != nil {
		s.data.Policy = previous
		s.data.ConfigRevision = previousRev
		return err
	}
	return nil
}

func (s *stateStore) listNodes() []*nodeRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*nodeRecord, 0, len(s.data.Nodes))
	for _, n := range s.data.Nodes {
		cp := *n
		cp.ProxyURL = n.ProxyURL
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *stateStore) getNode(id string) (*nodeRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.data.Nodes[id]
	if !ok {
		return nil, false
	}
	cp := *n
	cp.ProxyURL = n.ProxyURL
	return &cp, true
}

func (s *stateStore) createNode(name, proxyURL string, enabled, pool bool, capacity int) (*nodeRecord, error) {
	created, err := s.createNodes([]nodeCreateInput{{
		Name:            name,
		ProxyURL:        proxyURL,
		Enabled:         enabled,
		ProxyPool:       pool,
		AccountCapacity: capacity,
	}})
	if err != nil {
		return nil, err
	}
	return created[0], nil
}

func (s *stateStore) createNodes(inputs []nodeCreateInput) ([]*nodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(inputs) == 0 {
		return nil, fmt.Errorf("至少提供一个节点")
	}
	if len(inputs) > 500 {
		return nil, fmt.Errorf("单次最多导入 500 个节点")
	}
	for index := range inputs {
		inputs[index].Name = strings.TrimSpace(inputs[index].Name)
		inputs[index].ProxyURL = strings.TrimSpace(inputs[index].ProxyURL)
		if inputs[index].Name == "" || inputs[index].ProxyURL == "" {
			return nil, fmt.Errorf("第 %d 个节点缺少名称或代理 URL", index+1)
		}
		if err := validateProxyURL(inputs[index].ProxyURL); err != nil {
			return nil, fmt.Errorf("第 %d 个节点代理 URL 无效: %w", index+1, err)
		}
		if inputs[index].AccountCapacity < 0 || inputs[index].AccountCapacity > 100000 {
			return nil, fmt.Errorf("第 %d 个节点容量需在 0 到 100000 之间", index+1)
		}
	}
	previousNextID := s.data.NextID
	previousRev := s.data.ConfigRevision
	now := time.Now().UTC()
	created := make([]*nodeRecord, 0, len(inputs))
	createdIDs := make([]string, 0, len(inputs))
	for _, input := range inputs {
		id := fmt.Sprintf("%d", s.data.NextID)
		s.data.NextID++
		origin := strings.TrimSpace(input.Origin)
		if origin == "" {
			origin = nodeOriginManual
		}
		mode := strings.TrimSpace(input.ManagementMode)
		if mode == "" {
			mode = nodeModeManage
		}
		n := &nodeRecord{
			ID:              id,
			Name:            input.Name,
			ProxyURL:        input.ProxyURL,
			ProxyURLStored:  input.ProxyURL,
			Enabled:         input.Enabled,
			ProxyPool:       input.ProxyPool,
			AccountCapacity: input.AccountCapacity,
			Origin:          origin,
			ManagementMode:  mode,
			ProbeStatus:     "unknown",
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		s.data.Nodes[id] = n
		createdIDs = append(createdIDs, id)
		cp := *n
		created = append(created, &cp)
	}
	s.data.ConfigRevision++
	if err := s.persistLocked(); err != nil {
		for _, id := range createdIDs {
			delete(s.data.Nodes, id)
		}
		s.data.NextID = previousNextID
		s.data.ConfigRevision = previousRev
		return nil, err
	}
	return created, nil
}

func (s *stateStore) updateNode(id string, mut func(*nodeRecord) error) (*nodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.data.Nodes[id]
	if !ok {
		return nil, fmt.Errorf("节点不存在")
	}
	before := *n
	if err := mut(n); err != nil {
		return nil, err
	}
	if n.ProxyURL != "" {
		if err := validateProxyURL(n.ProxyURL); err != nil {
			*n = before
			return nil, err
		}
	}
	n.UpdatedAt = time.Now().UTC()
	critical := n.DisabledByGuard != before.DisabledByGuard ||
		n.QuarantinedUntil != before.QuarantinedUntil ||
		n.Enabled != before.Enabled ||
		n.ProxyURL != before.ProxyURL
	structural := n.Name != before.Name ||
		n.Enabled != before.Enabled ||
		n.ProxyURL != before.ProxyURL ||
		n.AccountCapacity != before.AccountCapacity ||
		n.ProxyPool != before.ProxyPool ||
		n.Origin != before.Origin ||
		n.ManagementMode != before.ManagementMode
	previousRev := s.data.ConfigRevision
	if structural {
		s.data.ConfigRevision++
	}
	var err error
	if critical || structural {
		err = s.flushNowLocked()
	} else {
		s.scheduleFlushLocked()
	}
	if err != nil {
		*n = before
		s.data.ConfigRevision = previousRev
		return nil, err
	}
	cp := *n
	cp.ProxyURL = n.ProxyURL
	return &cp, nil
}

func validateProxyURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("代理 URL 必须包含主机和端口")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return fmt.Errorf("代理协议仅支持 http、https、socks5 或 socks5h")
	}
}

func (s *stateStore) deleteNodes(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved := make(map[string]*nodeRecord, len(ids))
	for _, id := range ids {
		n, ok := s.data.Nodes[id]
		if !ok {
			continue
		}
		cp := *n
		saved[id] = &cp
		delete(s.data.Nodes, id)
	}
	if len(saved) == 0 {
		return nil
	}
	previousRev := s.data.ConfigRevision
	s.data.ConfigRevision++
	if err := s.persistLocked(); err != nil {
		for id, n := range saved {
			s.data.Nodes[id] = n
		}
		s.data.ConfigRevision = previousRev
		return err
	}
	return nil
}

func (s *stateStore) setBatchEnabled(ids []string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	type prev struct {
		enabled   bool
		updatedAt time.Time
	}
	saved := map[string]prev{}
	now := time.Now().UTC()
	for _, id := range ids {
		n, ok := s.data.Nodes[id]
		if !ok {
			continue
		}
		saved[id] = prev{enabled: n.Enabled, updatedAt: n.UpdatedAt}
		n.Enabled = enabled
		n.UpdatedAt = now
	}
	if len(saved) == 0 {
		return nil
	}
	previousRev := s.data.ConfigRevision
	s.data.ConfigRevision++
	if err := s.persistLocked(); err != nil {
		for id, before := range saved {
			if n, ok := s.data.Nodes[id]; ok {
				n.Enabled = before.enabled
				n.UpdatedAt = before.updatedAt
			}
		}
		s.data.ConfigRevision = previousRev
		return err
	}
	return nil
}

func (s *stateStore) appendEvent(ev guardEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.TS == 0 {
		ev.TS = float64(time.Now().Unix())
	}
	s.data.Events = append(s.data.Events, ev)
	if len(s.data.Events) > 100 {
		s.data.Events = s.data.Events[len(s.data.Events)-100:]
	}
	s.scheduleFlushLocked()
}

func (s *stateStore) events() []guardEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]guardEvent, len(s.data.Events))
	copy(out, s.data.Events)
	return out
}

func (s *stateStore) recordAuthObservation(authID, label, source, nodeID, nodeName, class, reason string, tps float64, degraded bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" || s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.AuthStats == nil {
		s.data.AuthStats = map[string]*authDegradeRecord{}
	}
	rec := s.data.AuthStats[authID]
	if rec == nil {
		rec = &authDegradeRecord{AuthID: authID}
		s.data.AuthStats[authID] = rec
	}
	if label != "" {
		rec.Label = label
	}
	rec.SampleCount++
	if degraded {
		rec.DegradedCount++
	}
	rec.LastAt = float64(time.Now().Unix())
	rec.LastSource = source
	if reason != "" {
		rec.LastReason = reason
	} else if degraded {
		rec.LastReason = "响应缺少 thinking_content（降智）"
	}
	if nodeID != "" {
		rec.LastNodeID = nodeID
	}
	if nodeName != "" {
		rec.LastNodeName = nodeName
	}
	if tps > 0 {
		rec.LastOutputTPS = tps
	}
	s.scheduleFlushLocked()
}

func authDegradeRate(rec *authDegradeRecord) float64 {
	if rec == nil || rec.SampleCount <= 0 {
		return 0
	}
	return float64(rec.DegradedCount) / float64(rec.SampleCount)
}

func (s *stateStore) listAuthDegradeStats() []*authDegradeRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*authDegradeRecord, 0, len(s.data.AuthStats))
	for _, rec := range s.data.AuthStats {
		if rec == nil {
			continue
		}
		cp := *rec
		out = append(out, &cp)
	}
	// 100% degrade rate first, then by degrade count, recency, id.
	sort.Slice(out, func(i, j int) bool {
		fullI := out[i].SampleCount > 0 && out[i].DegradedCount == out[i].SampleCount
		fullJ := out[j].SampleCount > 0 && out[j].DegradedCount == out[j].SampleCount
		if fullI != fullJ {
			return fullI
		}
		if out[i].DegradedCount != out[j].DegradedCount {
			return out[i].DegradedCount > out[j].DegradedCount
		}
		rateI, rateJ := authDegradeRate(out[i]), authDegradeRate(out[j])
		if rateI != rateJ {
			return rateI > rateJ
		}
		if out[i].LastAt != out[j].LastAt {
			return out[i].LastAt > out[j].LastAt
		}
		return out[i].AuthID < out[j].AuthID
	})
	return out
}

func (s *stateStore) clearAuthDegradeStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.data.AuthStats
	s.data.AuthStats = map[string]*authDegradeRecord{}
	if err := s.flushNowLocked(); err != nil {
		s.data.AuthStats = previous
	}
}

func (s *stateStore) stats() statistics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Stats
}

func (s *stateStore) bumpStat(source, class string, tokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ps *probeStats
	if source == "active" {
		ps = &s.data.Stats.Active
	} else {
		ps = &s.data.Stats.Passive
	}
	ps.Total++
	ps.OutputTokens += tokens
	switch class {
	case "healthy":
		ps.Healthy++
	case "soft":
		ps.Soft++
	case "hard":
		ps.Hard++
	case "error":
		ps.Errors++
	case "ignored", "account_error", "upstream_error", "no_account":
		ps.Ignored++
	}
	s.scheduleFlushLocked()
}

func (s *stateStore) bumpAction(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "quarantined":
		s.data.Stats.Actions.Quarantined++
	case "restored":
		s.data.Stats.Actions.Restored++
	case "suppressed":
		s.data.Stats.Actions.Suppressed++
	}
	s.scheduleFlushLocked()
}

func (s *stateStore) setAssignedCounts(counts map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, n := range s.data.Nodes {
		n.AssignedAccountCount = counts[id]
	}
	s.scheduleFlushLocked()
}

// nodeIDByProxy returns the node id bound to proxyURL (O(nodes), typically small).
func (s *stateStore) nodeIDByProxy(proxyURL string) string {
	if s == nil || proxyURL == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.data.Nodes {
		if n.ProxyURL == proxyURL {
			return n.ID
		}
	}
	return ""
}

func publicNode(n *nodeRecord) map[string]any {
	if n == nil {
		return nil
	}
	return map[string]any{
		"id":                   n.ID,
		"name":                 n.Name,
		"enabled":              n.Enabled,
		"proxyPool":            n.ProxyPool,
		"accountCapacity":      n.AccountCapacity,
		"exitIp":               n.ExitIP,
		"probeStatus":          n.ProbeStatus,
		"probeLatencyMs":       n.ProbeLatencyMs,
		"assignedAccountCount": n.AssignedAccountCount,
		"disabled_by_guard":    n.DisabledByGuard,
		"quarantined_until":    n.QuarantinedUntil,
		"error_strikes":        n.ErrorStrikes,
		"soft_strikes":         n.SoftStrikes,
		"thinking_strikes":     n.ThinkingStrikes,
		"last_classification":  n.LastClassification,
		"last_output_tps":      n.LastOutputTPS,
		"last_first_token_ms":  n.LastFirstTokenMs,
		"last_duration_ms":     n.LastDurationMs,
		"last_output_tokens":   n.LastOutputTokens,
		"last_reason":          n.LastReason,
		"last_source":          n.LastSource,
		"last_observed_at":     n.LastObservedAt,
		"last_probe_at":        n.LastProbeAt,
		"hasProxy":             n.ProxyURL != "",
		"origin":               n.Origin,
		"managementMode":       n.ManagementMode,
		"createdAt":            n.CreatedAt,
		"updatedAt":            n.UpdatedAt,
	}
}

func seedBuiltinProfiles(data *guardState) {
	if data.Profiles == nil {
		data.Profiles = map[string]*ProbeProfile{}
	}
	for _, builtin := range builtinProfiles() {
		cp := builtin
		existing, ok := data.Profiles[cp.ID]
		if !ok {
			cp.UpdatedAt = nowUnix()
			data.Profiles[cp.ID] = &cp
			continue
		}
		if existing.BuiltIn {
			existing.Name = cp.Name
			existing.Prompt = cp.Prompt
			existing.ExpectedText = cp.ExpectedText
			existing.MatchMode = cp.MatchMode
			existing.RequireThinking = cp.RequireThinking
			if existing.Temperature == 0 && cp.Temperature != 0 {
				existing.Temperature = cp.Temperature
			}
		}
	}
	if strings.TrimSpace(data.Policy.ActiveProfileID) == "" {
		data.Policy.ActiveProfileID = defaultProbeProfileID()
	}
	if _, ok := data.Profiles[data.Policy.ActiveProfileID]; !ok {
		data.Policy.ActiveProfileID = defaultProbeProfileID()
	}
}

func (s *stateStore) listProfiles() []ProbeProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProbeProfile, 0, len(s.data.Profiles))
	for _, p := range s.data.Profiles {
		if p == nil {
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BuiltIn != out[j].BuiltIn {
			return out[i].BuiltIn
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *stateStore) resolveProfile(id string) ProbeProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = s.data.Policy.ActiveProfileID
	}
	if p, ok := s.data.Profiles[id]; ok && p != nil {
		return *p
	}
	if p, ok := s.data.Profiles[defaultProbeProfileID()]; ok && p != nil {
		return *p
	}
	builtins := builtinProfiles()
	return builtins[0]
}

func (s *stateStore) createProfile(in ProbeProfile) (ProbeProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in.BuiltIn = false
	in.ID = ""
	if err := validateProbeProfile(&in, true); err != nil {
		return ProbeProfile{}, err
	}
	custom := 0
	for _, p := range s.data.Profiles {
		if p != nil && !p.BuiltIn {
			custom++
		}
	}
	if custom >= maxCustomProfiles {
		return ProbeProfile{}, fmt.Errorf("自定义方案最多 %d 个", maxCustomProfiles)
	}
	previousNextID := s.data.NextID
	previousRev := s.data.ConfigRevision
	s.data.NextID++
	in.ID = fmt.Sprintf("p-%d", s.data.NextID)
	in.UpdatedAt = nowUnix()
	if s.data.Profiles == nil {
		s.data.Profiles = map[string]*ProbeProfile{}
	}
	cp := in
	s.data.Profiles[in.ID] = &cp
	s.data.ConfigRevision++
	if err := s.persistLocked(); err != nil {
		delete(s.data.Profiles, in.ID)
		s.data.NextID = previousNextID
		s.data.ConfigRevision = previousRev
		return ProbeProfile{}, err
	}
	return in, nil
}

func (s *stateStore) updateProfile(id string, in ProbeProfile) (ProbeProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.data.Profiles[id]
	if !ok || existing == nil {
		return ProbeProfile{}, fmt.Errorf("方案不存在")
	}
	if existing.BuiltIn {
		return ProbeProfile{}, fmt.Errorf("内置方案不能修改，请复制后另存")
	}
	in.ID = existing.ID
	in.BuiltIn = false
	if err := validateProbeProfile(&in, false); err != nil {
		return ProbeProfile{}, err
	}
	in.UpdatedAt = nowUnix()
	previous := *existing
	previousRev := s.data.ConfigRevision
	cp := in
	s.data.Profiles[id] = &cp
	s.data.ConfigRevision++
	if err := s.persistLocked(); err != nil {
		s.data.Profiles[id] = &previous
		s.data.ConfigRevision = previousRev
		return ProbeProfile{}, err
	}
	return in, nil
}

func (s *stateStore) deleteProfile(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.data.Profiles[id]
	if !ok || existing == nil {
		return fmt.Errorf("方案不存在")
	}
	if existing.BuiltIn {
		return fmt.Errorf("内置方案不能删除")
	}
	previousPolicyID := s.data.Policy.ActiveProfileID
	previousRev := s.data.ConfigRevision
	saved := *existing
	if s.data.Policy.ActiveProfileID == id {
		s.data.Policy.ActiveProfileID = defaultProbeProfileID()
	}
	delete(s.data.Profiles, id)
	s.data.ConfigRevision++
	if err := s.persistLocked(); err != nil {
		s.data.Profiles[id] = &saved
		s.data.Policy.ActiveProfileID = previousPolicyID
		s.data.ConfigRevision = previousRev
		return err
	}
	return nil
}
