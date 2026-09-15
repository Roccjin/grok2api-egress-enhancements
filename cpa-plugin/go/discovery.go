package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	discoveryScanConcurrency = 4
	discoveryOpTTL           = 30 * time.Minute
	discoveryMaxAuths        = 10000
)

type discoveryCandidate struct {
	ID                string   `json:"id"`
	HostPreview       string   `json:"hostPreview"`
	Scheme            string   `json:"scheme"`
	AuthCount         int      `json:"authCount"`
	EnabledAuthCount  int      `json:"enabledAuthCount"`
	DisabledAuthCount int      `json:"disabledAuthCount"`
	ExistingNodeID    string   `json:"existingNodeID,omitempty"`
	ExistingNodeIDs   []string `json:"existingNodeIDs,omitempty"`
	Status            string   `json:"status"`
	Reason            string   `json:"reason,omitempty"`
	proxyURL          string
	identity          string
}

type discoverySummary struct {
	AuthTotal      int   `json:"authTotal"`
	XAIAuths       int   `json:"xaiAuths"`
	UniqueProxies  int   `json:"uniqueProxies"`
	NewNodes       int   `json:"newNodes"`
	AlreadyExists  int   `json:"alreadyExists"`
	Conflicts      int   `json:"conflicts"`
	Skipped        int   `json:"skipped"`
	Failed         int   `json:"failed"`
	Unsupported    int   `json:"unsupported"`
	Complete       bool  `json:"complete"`
	ConfigRevision int64 `json:"configRevision"`
}

type discoveryOp struct {
	ID         string
	Status     string
	Phase      string
	Error      string
	Summary    discoverySummary
	Candidates []*discoveryCandidate
	CreatedAt  time.Time
	cancel     atomic.Bool
}

var (
	discoveryMu  sync.Mutex
	discoveryOps = map[string]*discoveryOp{}
	discoverySeq atomic.Uint64
)

func proxyIdentityV1(proxyURL string) string {
	trimmed := strings.TrimSpace(proxyURL)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}

func redactProxyPreview(proxyURL string) (scheme, host string) {
	u, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || u.Host == "" {
		return "", ""
	}
	host = u.Hostname()
	if u.Port() != "" {
		host = host + ":" + u.Port()
	}
	return strings.ToLower(u.Scheme), host
}

func classifyXAIEntry(f pluginapi.HostAuthFileEntry) (ok bool, reason string) {
	if f.RuntimeOnly {
		return false, "runtime_only"
	}
	prov := strings.ToLower(strings.TrimSpace(f.Provider))
	typ := strings.ToLower(strings.TrimSpace(f.Type))
	if prov == "xai" || typ == "xai" {
		return true, "provider"
	}
	if prov != "" || typ != "" {
		return false, "not_xai"
	}
	name := strings.ToLower(strings.TrimSpace(f.Name))
	if strings.HasPrefix(name, "xai-") || strings.Contains(name, "/xai-") {
		return true, "filename"
	}
	return false, "not_xai"
}

func startNodeDiscovery() (*discoveryOp, error) {
	ensureStore()
	if store == nil {
		return nil, fmt.Errorf("状态存储不可用")
	}
	op := &discoveryOp{
		ID:        fmt.Sprintf("discovery-%d", discoverySeq.Add(1)),
		Status:    "running",
		Phase:     "scan",
		CreatedAt: time.Now().UTC(),
		Summary:   discoverySummary{ConfigRevision: store.snapshot().ConfigRevision},
	}
	discoveryMu.Lock()
	now := time.Now()
	for id, existing := range discoveryOps {
		if now.Sub(existing.CreatedAt) > discoveryOpTTL {
			delete(discoveryOps, id)
		}
	}
	discoveryOps[op.ID] = op
	discoveryMu.Unlock()
	goTracked(func() { runNodeDiscovery(op) })
	return op, nil
}

func getDiscoveryOp(id string) *discoveryOp {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	return discoveryOps[strings.TrimSpace(id)]
}

func publicDiscoveryOp(op *discoveryOp) map[string]any {
	if op == nil {
		return nil
	}
	return map[string]any{
		"id":      op.ID,
		"status":  op.Status,
		"phase":   op.Phase,
		"error":   op.Error,
		"summary": op.Summary,
	}
}

func publicDiscoveryCandidate(c *discoveryCandidate) map[string]any {
	if c == nil {
		return nil
	}
	return map[string]any{
		"id":                c.ID,
		"hostPreview":       c.HostPreview,
		"scheme":            c.Scheme,
		"authCount":         c.AuthCount,
		"enabledAuthCount":  c.EnabledAuthCount,
		"disabledAuthCount": c.DisabledAuthCount,
		"existingNodeID":    c.ExistingNodeID,
		"existingNodeIDs":   c.ExistingNodeIDs,
		"status":            c.Status,
		"reason":            c.Reason,
	}
}

func runNodeDiscovery(op *discoveryOp) {
	defer func() {
		if recovered := recover(); recovered != nil {
			op.Status = "failed"
			op.Error = "scan panicked"
		}
	}()
	entries, err := listHostAuthEntries()
	if err != nil {
		op.Status = "failed"
		op.Phase = "list"
		op.Error = "读取凭证列表失败"
		return
	}
	if len(entries) > discoveryMaxAuths {
		entries = entries[:discoveryMaxAuths]
	}
	op.Summary.AuthTotal = len(entries)

	type job struct {
		entry pluginapi.HostAuthFileEntry
	}
	jobs := make(chan job)
	type result struct {
		skipped     bool
		failed      bool
		unsupported bool
		notXAI      bool
		file        authFile
		reason      string
	}
	results := make(chan result)
	workers := discoveryScanConcurrency
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if op.cancel.Load() {
					return
				}
				okXAI, why := classifyXAIEntry(j.entry)
				if !okXAI {
					if why == "runtime_only" {
						results <- result{unsupported: true, reason: why}
					} else {
						results <- result{notXAI: true, reason: why}
					}
					continue
				}
				idx := strings.TrimSpace(j.entry.AuthIndex)
				if idx == "" {
					results <- result{unsupported: true, reason: "missing_auth_index"}
					continue
				}
				got, errGet := getAuthFileByIndex(idx)
				if errGet != nil {
					results <- result{failed: true, reason: "auth_get_failed"}
					continue
				}
				got.Disabled = got.Disabled || j.entry.Disabled
				results <- result{file: got, reason: why}
			}
		}()
	}
	go func() {
		for _, entry := range entries {
			if op.cancel.Load() {
				break
			}
			jobs <- job{entry: entry}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	byIdentity := map[string]*discoveryCandidate{}
	order := make([]string, 0)
	skipped := 0
	failed := 0
	unsupported := 0
	xai := 0
	for res := range results {
		if op.cancel.Load() {
			op.Status = "cancelled"
			op.Phase = "scan"
			return
		}
		if res.notXAI {
			skipped++
			continue
		}
		if res.unsupported {
			unsupported++
			continue
		}
		if res.failed {
			failed++
			continue
		}
		xai++
		proxy := strings.TrimSpace(res.file.ProxyURL)
		if proxy == "" {
			skipped++
			continue
		}
		if err := validateProxyURL(proxy); err != nil {
			skipped++
			continue
		}
		ident := proxyIdentityV1(proxy)
		cand := byIdentity[ident]
		if cand == nil {
			scheme, host := redactProxyPreview(proxy)
			cand = &discoveryCandidate{
				ID:          fmt.Sprintf("cand-%d", len(order)+1),
				HostPreview: host,
				Scheme:      scheme,
				Status:      "new",
				proxyURL:    proxy,
				identity:    ident,
			}
			byIdentity[ident] = cand
			order = append(order, ident)
		}
		cand.AuthCount++
		if res.file.Disabled {
			cand.DisabledAuthCount++
		} else {
			cand.EnabledAuthCount++
		}
	}

	existing := store.proxyIdentityIndex()
	ignored := store.ignoredIdentitySet()
	candidates := make([]*discoveryCandidate, 0, len(order))
	newCount := 0
	existsCount := 0
	conflictCount := 0
	for _, ident := range order {
		cand := byIdentity[ident]
		ids := existing[ident]
		switch {
		case ignored[ident]:
			cand.Status = "skipped"
			cand.Reason = "ignored"
			skipped++
		case len(ids) > 1:
			cand.Status = "conflict"
			cand.ExistingNodeIDs = append([]string(nil), ids...)
			cand.ExistingNodeID = ids[0]
			cand.Reason = "multiple_nodes"
			conflictCount++
		case len(ids) == 1:
			cand.Status = "already_exists"
			cand.ExistingNodeID = ids[0]
			existsCount++
		default:
			cand.Status = "new"
			newCount++
		}
		candidates = append(candidates, cand)
	}

	op.Candidates = candidates
	op.Summary.XAIAuths = xai
	op.Summary.UniqueProxies = len(order)
	op.Summary.NewNodes = newCount
	op.Summary.AlreadyExists = existsCount
	op.Summary.Conflicts = conflictCount
	op.Summary.Skipped = skipped
	op.Summary.Failed = failed
	op.Summary.Unsupported = unsupported
	op.Summary.Complete = failed == 0
	op.Summary.ConfigRevision = store.snapshot().ConfigRevision
	op.Phase = "preview"
	op.Status = "succeeded"
}

func listHostAuthEntries() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, mustJSON(map[string]any{}))
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		var files []pluginapi.HostAuthFileEntry
		if err2 := json.Unmarshal(raw, &files); err2 != nil {
			return nil, fmt.Errorf("decode auth list: %w", err)
		}
		resp.Files = files
	}
	return resp.Files, nil
}

func getAuthFileByIndex(authIndex string) (authFile, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthGet, mustJSON(map[string]any{"auth_index": authIndex}))
	if err != nil {
		return authFile{}, err
	}
	var resp hostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return authFile{}, err
	}
	obj := map[string]any{}
	if len(resp.JSON) > 0 {
		_ = json.Unmarshal(resp.JSON, &obj)
	}
	email, _ := obj["email"].(string)
	proxy, _ := obj["proxy_url"].(string)
	disabled, _ := obj["disabled"].(bool)
	name := resp.Name
	idx := resp.AuthIndex
	if idx == "" {
		idx = authIndex
	}
	return authFile{
		Index:    idx,
		Name:     name,
		Path:     resp.Path,
		Email:    email,
		Disabled: disabled,
		ProxyURL: strings.TrimSpace(proxy),
		Raw:      obj,
	}, nil
}

func pageDiscoveryCandidates(op *discoveryOp, page, pageSize int) (items []map[string]any, total, outPage, totalPages int) {
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 100 {
		pageSize = 100
	}
	total = len(op.Candidates)
	totalPages = 1
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	items = make([]map[string]any, 0, end-start)
	for _, cand := range op.Candidates[start:end] {
		items = append(items, publicDiscoveryCandidate(cand))
	}
	return items, total, page, totalPages
}

func commitDiscovery(op *discoveryOp, selection string, expectedRev int64) (map[string]any, error) {
	if op == nil {
		return nil, fmt.Errorf("发现任务不存在")
	}
	if op.Status != "succeeded" {
		return nil, fmt.Errorf("发现任务尚未完成")
	}
	if expectedRev != 0 && store.snapshot().ConfigRevision != expectedRev {
		return nil, fmt.Errorf("配置已变化，请重新扫描")
	}
	inputs := make([]nodeCreateInput, 0)
	switch strings.TrimSpace(selection) {
	case "", "all_confirmed", "new":
		for _, cand := range op.Candidates {
			if cand.Status != "new" {
				continue
			}
			inputs = append(inputs, nodeCreateInput{
				Name:            "",
				ProxyURL:        cand.proxyURL,
				Enabled:         true,
				ProxyPool:       false,
				AccountCapacity: 0,
				Origin:          nodeOriginDiscovery,
				ManagementMode:  nodeModeObserve,
			})
		}
	default:
		return nil, fmt.Errorf("不支持的选择模式")
	}
	created, err := store.upsertDiscoveredNodes(inputs)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"created":       created,
		"alreadyExists": op.Summary.AlreadyExists,
		"conflicts":     op.Summary.Conflicts,
		"skipped":       op.Summary.Skipped,
		"failed":        op.Summary.Failed,
	}, nil
}

func parsePositiveInt(raw string, fallback int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("must be a positive integer")
	}
	return n, nil
}
