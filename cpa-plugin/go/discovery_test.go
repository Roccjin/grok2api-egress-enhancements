package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDiscoveryCreatesObserveNodesWithoutSavingAuth(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	saved := 0
	original := hostCall
	hostCall = func(method string, payload []byte) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return json.Marshal(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
				{AuthIndex: "0", Name: "xai-a.json", Provider: "xai", Type: "xai"},
				{AuthIndex: "1", Name: "xai-b.json", Provider: "xai", Type: "xai"},
				{AuthIndex: "2", Name: "codex.json", Provider: "codex", Type: "codex"},
			}})
		case pluginabi.MethodHostAuthGet:
			var request map[string]string
			_ = json.Unmarshal(payload, &request)
			idx := request["auth_index"]
			proxy := "socks5://127.0.0.1:7951"
			if idx == "1" {
				proxy = "socks5://127.0.0.1:7952"
			}
			body, _ := json.Marshal(map[string]any{"type": "xai", "proxy_url": proxy, "disabled": false})
			return json.Marshal(hostAuthGetResponse{AuthIndex: idx, Name: "xai-" + idx + ".json", JSON: body})
		case pluginabi.MethodHostAuthSave:
			saved++
			return json.Marshal(map[string]any{})
		default:
			return nil, nil
		}
	}
	defer func() { hostCall = original }()

	op, err := startNodeDiscovery()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && op.Status == "running" {
		time.Sleep(20 * time.Millisecond)
	}
	if op.Status != "succeeded" {
		t.Fatalf("status=%s err=%s", op.Status, op.Error)
	}
	if op.Summary.NewNodes != 2 || op.Summary.XAIAuths != 2 {
		t.Fatalf("summary=%+v", op.Summary)
	}
	result, err := commitDiscovery(op, "all_confirmed", op.Summary.ConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	if result["created"] != 2 {
		t.Fatalf("created=%v", result["created"])
	}
	if saved != 0 {
		t.Fatalf("host.auth.save called %d times", saved)
	}
	nodes := store.listNodes()
	if len(nodes) != 2 {
		t.Fatalf("nodes=%d", len(nodes))
	}
	for _, n := range nodes {
		if n.Origin != nodeOriginDiscovery || n.ManagementMode != nodeModeObserve {
			t.Fatalf("node %+v", n)
		}
		if n.ProxyURL == "" {
			t.Fatal("missing proxy")
		}
	}
	again, err := store.upsertDiscoveredNodes([]nodeCreateInput{{
		ProxyURL: "socks5://127.0.0.1:7951", Enabled: true,
	}})
	if err != nil || again != 0 {
		t.Fatalf("idempotent upsert created=%d err=%v", again, err)
	}
}

func TestDispatchNodesPageQuery(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	for i := 1; i <= 3; i++ {
		if _, err := store.createNode(fmt.Sprintf("n%d", i), fmt.Sprintf("http://127.0.0.1:%d", i), true, false, 0); err != nil {
			t.Fatal(err)
		}
	}
	headers := make(http.Header)
	headers.Set("X-Grok2API-Egress-UI", "1")
	body, _ := json.Marshal(uiProxyRequest{Method: http.MethodGet, Path: "/nodes?page=1&pageSize=2&sort=id"})
	raw, err := handleUIProxy(managementRequest{Method: http.MethodPost, Headers: headers, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	var resp managementResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Body)
	}
	var payload map[string]any
	_ = json.Unmarshal(resp.Body, &payload)
	data, _ := payload["data"].(map[string]any)
	if data["pageSize"] != float64(2) || data["total"] != float64(3) {
		t.Fatalf("payload=%s", resp.Body)
	}
	items, _ := data["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items=%d body=%s", len(items), resp.Body)
	}

	body, _ = json.Marshal(uiProxyRequest{Method: http.MethodGet, Path: "/nodes?page=1&pageSize=999"})
	raw, err = handleUIProxy(managementRequest{Method: http.MethodPost, Headers: headers, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &env)
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 got %d %s", resp.StatusCode, resp.Body)
	}
}

func TestParseNodeListQueryRejectsUnknownSort(t *testing.T) {
	_, err := parseNodeListQuery(url.Values{"sort": []string{"nope"}})
	if err == nil {
		t.Fatal("expected invalid sort")
	}
}
