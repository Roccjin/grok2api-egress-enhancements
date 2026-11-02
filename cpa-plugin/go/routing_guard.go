package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleSchedulerPick hands auth selection entirely back to the host.
//
// CPA only consults SessionAffinitySelector on the legacy path when the plugin
// responds Handled:false (conductor.pickNextLegacy: "if !handled { selector.Pick }").
// Any Handled:true answer — pinning AuthID or DelegateBuiltin — routes into
// pickViaBuiltinScheduler, a bare round-robin cursor that bypasses session
// affinity entirely and rotates auths on every request.
func handleSchedulerPick(request []byte) ([]byte, error) {
	_ = request
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
}

// handleRequestInterceptAfterAuth closes the small race between auth selection
// and synchronous quarantine migration. A request selected during that window
// receives a retryable response instead of reaching a known bad egress.
func handleRequestIntercept(request []byte, afterAuth bool) ([]byte, error) {
	ensureStore()
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode request interceptor request: %w", err)
	}
	if !afterAuth {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	meta := thinkingRetryMetadata()
	if len(req.Metadata) == 0 {
		return okEnvelope(interceptResponse{Metadata: meta})
	}
	selected := firstString(req.Metadata, "selected_auth_id", "selectedAuthID", "auth_id", "authID")
	if selected == "" {
		return okEnvelope(interceptResponse{Metadata: meta})
	}
	nodeID := resolveNodeIDForAuth(store, selected)
	if nodeID == "" {
		return okEnvelope(interceptResponse{Metadata: meta})
	}
	node, ok := store.getNode(nodeID)
	if !ok || !node.DisabledByGuard {
		return okEnvelope(interceptResponse{Metadata: meta})
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "egress_quarantined",
			"message": "当前账号出口正在隔离迁移，请重试",
		},
	})
	return okEnvelope(pluginapi.RequestInterceptResponse{
		Terminate:  true,
		StatusCode: http.StatusServiceUnavailable,
		ResponseHeaders: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"1"},
		},
		ResponseBody: body,
	})
}

// interceptResponse adds Metadata on top of the host intercept response.
// The plugin module's RequestInterceptResponse predates that field.
type interceptResponse struct {
	pluginapi.RequestInterceptResponse
	Metadata map[string]any `json:"Metadata,omitempty"`
}

func thinkingRetryMetadata() map[string]any {
	if store == nil {
		return nil
	}
	pol := store.policy()
	retry := pol.ThinkingRetry
	if !pol.ThinkingGuard || retry < 0 {
		retry = 0
	}
	if retry > 0 && pol.MinHealthyNodes > 0 && healthyNodeCount(store) < pol.MinHealthyNodes {
		retry = 0
	}
	minTokens := pol.MinOutputTokens
	if minTokens <= 0 {
		minTokens = 32
	}
	return map[string]any{
		"egress_thinking_retry":             retry,
		"egress_thinking_min_output_tokens": minTokens,
	}
}

func healthyNodeCount(store *stateStore) int {
	if store == nil {
		return 0
	}
	count := 0
	for _, node := range store.listNodes() {
		if node.Enabled && !node.DisabledByGuard && strings.TrimSpace(node.ProxyURL) != "" {
			count++
		}
	}
	return count
}
