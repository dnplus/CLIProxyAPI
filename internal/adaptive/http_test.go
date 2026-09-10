package adaptive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type observedUsage struct {
	TaskID string
	Record usage.Record
}
type testUsageObserver chan observedUsage

func (o testUsageObserver) HandleUsage(ctx context.Context, r usage.Record) {
	id, _ := ctx.Value(taskContextKey{}).(string)
	if id == "" {
		return
	}
	select {
	case o <- observedUsage{id, r}:
	default:
	}
}

func TestNativeHTTPJourneyStreamingToolsAffinityAndRestart(t *testing.T) {
	observed := make(testUsageObserver, 8)
	usage.RegisterNamedPlugin("adaptive-test", observed)
	gin.SetMode(gin.TestMode)
	s := fixture(t)
	e := engine(t, s)
	var mu sync.Mutex
	var authHeaders []string
	var bodies []map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		bodies = append(bodies, body)
		mu.Unlock()
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"4\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
		}
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(0, 0, 1)
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor("fixture-api", cfg))
	for _, r := range s.Config.Routes {
		a := &coreauth.Auth{ID: r.AuthID, Provider: r.Provider, Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": r.AuthID, "base_url": upstream.URL}}
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(r.AuthID, r.Provider, []*registry.ModelInfo{{ID: "fixture-strong"}, {ID: "fixture-economy"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(r.AuthID) })
	}
	path := filepath.Join(t.TempDir(), "state.json")
	h := &HTTP{Engine: e, StatePath: path, Base: handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager), Now: func() time.Time { return s.Events[0].At }}
	g := gin.New()
	g.Use(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer fixture-client" {
			c.AbortWithStatus(401)
			return
		}
		c.Set("userApiKey", "fixture-client")
		c.Next()
	})
	h.Routes(g.Group("/adaptive"))
	front := httptest.NewServer(g)
	defer front.Close()
	call := func(path string, value any, authorized bool) (int, string) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, front.URL+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer fixture-client")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	task := *s.Events[0].Task
	request := map[string]any{"model": "adaptive", "messages": []map[string]any{{"role": "assistant", "tool_calls": []map[string]any{{"id": "call-1", "type": "function", "function": map[string]any{"name": "sum", "arguments": "{\"a\":2,\"b\":2}"}}}}, {"role": "tool", "tool_call_id": "call-1", "content": "4"}}, "tools": []map[string]any{{"type": "function", "function": map[string]any{"name": "sum", "parameters": map[string]any{"type": "object"}}}}, "max_tokens": 100}
	envelope := map[string]any{"task": task, "request": request}
	if status, _ := call("/adaptive/shadow", task, false); status != 401 {
		t.Fatal(status)
	}
	if status, body := call("/adaptive/shadow", task, true); status != 200 || !strings.Contains(body, `"selected":"economy"`) {
		t.Fatalf("shadow: %d %s", status, body)
	}
	if len(e.State.Reservations) != 0 {
		t.Fatal("shadow reserved quota")
	}
	if status, _ := call("/adaptive/execute", envelope, true); status != 403 {
		t.Fatal(status)
	}
	h.ExecuteEnabled = true
	if status, body := call("/adaptive/execute", envelope, true); status != 200 || !strings.Contains(body, `"content":"4"`) {
		t.Fatalf("execute: %d %s", status, body)
	}
	mu.Lock()
	if len(authHeaders) != 1 || authHeaders[0] != "Bearer fixture-account-b" || string(bodies[0]["model"]) != `"fixture-economy"` || !bytes.Contains(bodies[0]["messages"], []byte("call-1")) || len(bodies[0]["tools"]) == 0 {
		t.Fatalf("native route/pin/tools failed: %v %s", authHeaders, bodies)
	}
	mu.Unlock()
	select {
	case event := <-observed:
		if !strings.HasSuffix(event.TaskID, ":task-1") || event.Record.AuthID != "fixture-account-b" || event.Record.Detail.TotalTokens != 11 || event.Record.Failed {
			t.Fatalf("uncorrelated usage: %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing native usage observation")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(s.Config, state)
	if err != nil {
		t.Fatal(err)
	}
	h.Engine = reloaded
	if status, body := call("/adaptive/execute", envelope, true); status != 503 || !strings.Contains(body, "duplicate_task") {
		t.Fatalf("duplicate: %d %s", status, body)
	}
	task.ID = "second"
	estimate := task.Estimates["strong"]
	estimate.CostUSD = 0.5
	task.Estimates["strong"] = estimate
	envelope["task"] = task
	request["stream"] = true
	if status, body := call("/adaptive/execute", envelope, true); status != 200 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream: %d %s", status, body)
	}
	mu.Lock()
	if len(authHeaders) != 2 || authHeaders[1] != "Bearer fixture-account-b" {
		t.Fatalf("affinity: %v", authHeaders)
	}
	mu.Unlock()
	a, _ := manager.GetByID("fixture-account-b")
	a.Disabled = true
	if _, err = manager.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	task.ID = "third"
	envelope["task"] = task
	if status, _ := call("/adaptive/execute", envelope, true); status != 503 {
		t.Fatal(status)
	}
	task.ID = "failed-save"
	task.Session = "new-session"
	envelope["task"] = task
	h.StatePath = filepath.Join(t.TempDir(), "missing", "state.json")
	if status, body := call("/adaptive/execute", envelope, true); status != 503 || !strings.Contains(body, "reservation_not_durable") {
		t.Fatalf("save failure: %d %s", status, body)
	}
	if !h.fault {
		t.Fatal("save failure must latch closed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(authHeaders) != 2 {
		t.Fatal("unavailable session switched accounts")
	}
}

func TestHTTPBusyAndFaultRejectImmediately(t *testing.T) {
	s := fixture(t)
	e := engine(t, s)
	h := &HTTP{Engine: e, StatePath: filepath.Join(t.TempDir(), "missing", "state"), ExecuteEnabled: true}
	h.mu.Lock()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if h.lock(c) {
		t.Fatal("busy lock")
	}
	h.mu.Unlock()
	h.fault = true
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	if h.lock(c) {
		t.Fatal("fault lock")
	}
}

func TestNativeFixedRouteUsesExistingCredentialFailover(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("Authorization")
		seen = append(seen, id)
		w.Header().Set("Content-Type", "application/json")
		if id == "Bearer baseline-a" {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"message":"fixture quota exceeded"}}`)
			return
		}
		io.WriteString(w, `{"id":"fixture","choices":[{"message":{"role":"assistant","content":"accepted"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(0, 0, 2)
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor("baseline-fixture", cfg))
	for _, id := range []string{"baseline-a", "baseline-b"} {
		a := &coreauth.Auth{ID: id, Provider: "baseline-fixture", Status: coreauth.StatusActive, Attributes: map[string]string{"api_key": id, "base_url": upstream.URL}}
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, a.Provider, []*registry.ModelInfo{{ID: "baseline-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	response, err := manager.Execute(context.Background(), []string{"baseline-fixture"}, coreexecutor.Request{Model: "baseline-model", Payload: []byte(`{"model":"baseline-model","messages":[{"role":"user","content":"fixture"}]}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "Bearer baseline-a,Bearer baseline-b" || !bytes.Contains(response.Payload, []byte("accepted")) {
		t.Fatalf("native failover: %v %s", seen, response.Payload)
	}
}
