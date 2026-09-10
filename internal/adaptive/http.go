package adaptive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type routeContextKey struct{}

type NativeRouter struct{}

func (NativeRouter) HasModelRouters() bool { return true }

func (NativeRouter) RouteModel(ctx context.Context, _ pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	r, ok := ctx.Value(routeContextKey{}).(Route)
	return pluginapi.ModelRouteResponse{Handled: ok, TargetKind: pluginapi.ModelRouteTargetProvider, Target: r.Provider, TargetModel: r.Model, Reason: "adaptive_reserved_route"}, ok
}

type HTTP struct {
	Engine         *Engine
	StatePath      string
	ExecuteEnabled bool
	Base           *handlers.BaseAPIHandler
	Now            func() time.Time
	mu             sync.Mutex
	fault          bool
}

type executionEnvelope struct {
	Task    Task            `json:"task"`
	Request json.RawMessage `json:"request"`
}

func (h *HTTP) Routes(g *gin.RouterGroup) {
	g.POST("/shadow", h.shadow)
	g.POST("/observe", h.observe)
	g.POST("/execute", h.execute)
	g.GET("/accounts", h.accounts)
}

func (h *HTTP) clock() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now().UTC()
}

func decodeBody(c *gin.Context, v any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("single JSON object required")
	}
	return nil
}

func (h *HTTP) lock(c *gin.Context) bool {
	if !h.mu.TryLock() {
		c.JSON(409, gin.H{"error": "router_busy"})
		return false
	}
	if h.fault {
		h.mu.Unlock()
		c.JSON(503, gin.H{"error": "state_write_failed_restart_required"})
		return false
	}
	return true
}

func scopeTask(c *gin.Context, t *Task) {
	sum := sha256.Sum256([]byte(c.GetString("userApiKey")))
	prefix := hex.EncodeToString(sum[:8]) + ":"
	if t.Session != "" {
		t.Session = prefix + t.Session
	}
	if t.ID != "" {
		t.ID = prefix + t.ID
	}
}

func (h *HTTP) available() map[string]bool {
	available := map[string]bool{}
	if h.Base == nil || h.Base.AuthManager == nil {
		return available
	}
	for _, r := range h.Engine.Config.Routes {
		a, ok := h.Base.AuthManager.GetByID(r.AuthID)
		available[r.ID] = ok && a != nil && !a.Disabled && a.Provider == r.Provider && a.Attributes["api_key"] != ""
	}
	return available
}

func (h *HTTP) shadow(c *gin.Context) {
	var t Task
	if err := decodeBody(c, &t); err != nil {
		c.JSON(400, gin.H{"error": "invalid_task"})
		return
	}
	scopeTask(c, &t)
	if !h.lock(c) {
		return
	}
	defer h.mu.Unlock()
	d := h.Engine.Decide(t, h.clock(), "adaptive", h.available())
	c.JSON(200, d)
}

func (h *HTTP) observe(c *gin.Context) {
	var o Observation
	if err := decodeBody(c, &o); err != nil {
		c.JSON(400, gin.H{"error": "invalid_observation"})
		return
	}
	if o.At.After(h.clock()) {
		c.JSON(400, gin.H{"error": "future_observation"})
		return
	}
	if !h.lock(c) {
		return
	}
	defer h.mu.Unlock()
	if err := h.Engine.Observe(o); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if err := h.Engine.Save(h.StatePath); err != nil {
		h.fault = true
		c.JSON(503, gin.H{"error": "state_write_failed"})
		return
	}
	c.JSON(200, gin.H{"accepted": true})
}

func (h *HTTP) execute(c *gin.Context) {
	if !h.ExecuteEnabled {
		c.JSON(403, gin.H{"error": "execution_requires_enable_execute_flag"})
		return
	}
	var envelope executionEnvelope
	if err := decodeBody(c, &envelope); err != nil {
		c.JSON(400, gin.H{"error": "invalid_envelope"})
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Request, &payload); err != nil || payload == nil {
		c.JSON(400, gin.H{"error": "invalid_request"})
		return
	}
	if len(payload["messages"]) == 0 || len(payload["previous_response_id"]) > 0 || len(payload["input"]) > 0 {
		c.JSON(400, gin.H{"error": "v1_requires_explicit_chat_messages"})
		return
	}
	var messages []struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		ToolCalls json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(payload["messages"], &messages); err != nil || len(messages) == 0 {
		c.JSON(400, gin.H{"error": "invalid_messages"})
		return
	}
	needsTools := len(payload["tools"]) > 0 || len(payload["functions"]) > 0
	needsVision := false
	for _, m := range messages {
		needsTools = needsTools || m.Role == "tool" || len(m.ToolCalls) > 0
		needsVision = needsVision || bytes.Contains(m.Content, []byte(`"image_url"`))
	}
	if needsTools && !slices.Contains(envelope.Task.Capabilities, "tools") {
		envelope.Task.Capabilities = append(envelope.Task.Capabilities, "tools")
	}
	if needsVision && !slices.Contains(envelope.Task.Capabilities, "vision") {
		envelope.Task.Capabilities = append(envelope.Task.Capabilities, "vision")
	}
	scopeTask(c, &envelope.Task)
	if !h.lock(c) {
		return
	}
	defer h.mu.Unlock()
	d := h.Engine.Decide(envelope.Task, h.clock(), "adaptive", h.available())
	if d.Selected == "" {
		c.JSON(503, d)
		return
	}
	r := h.Engine.Route(d.Selected)
	if err := h.Engine.Reserve(envelope.Task, d); err != nil {
		c.JSON(409, gin.H{"error": "reservation_conflict"})
		return
	}
	if err := h.Engine.Save(h.StatePath); err != nil {
		h.fault = true
		c.JSON(503, gin.H{"error": "reservation_not_durable"})
		return
	}
	payload["model"], _ = json.Marshal(r.Model)
	body, err := json.Marshal(payload)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid_payload"})
		return
	}
	ctx := handlers.WithPinnedAuthID(c.Request.Context(), r.AuthID)
	ctx = context.WithValue(ctx, routeContextKey{}, r)
	c.Request = c.Request.WithContext(ctx)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	c.Header("X-Adaptive-Route", r.ID)
	log.WithFields(log.Fields{"task_id": envelope.Task.ID, "route_id": r.ID, "reason": d.Reason, "evidence": "estimate", "cost_usd": envelope.Task.Estimates[r.ID].CostUSD}).Info("adaptive_route_reserved")
	base := handlers.NewBaseAPIHandlers(h.Base.Cfg, h.Base.AuthManager)
	base.ModelRouterHost = NativeRouter{}
	openai.NewOpenAIAPIHandler(base).ChatCompletions(c)
}

func (h *HTTP) accounts(c *gin.Context) {
	rows := []map[string]any{}
	if h.Base != nil && h.Base.AuthManager != nil {
		for _, a := range h.Base.AuthManager.List() {
			if a.Attributes["api_key"] != "" {
				rows = append(rows, map[string]any{"auth_id": a.ID, "provider": a.Provider, "disabled": a.Disabled})
			}
		}
	}
	c.JSON(200, rows)
}

type UsageObserver struct{}

func (UsageObserver) HandleUsage(_ context.Context, r usage.Record) {
	log.WithFields(log.Fields{"provider": r.Provider, "model": r.Model, "auth_id": r.AuthID, "latency_ms": r.Latency.Milliseconds(), "ttft_ms": r.TTFT.Milliseconds(), "failed": r.Failed, "tokens": r.Detail.TotalTokens, "quota_snapshot": false, "acceptance_verified": false}).Info("adaptive_usage_observed")
}
