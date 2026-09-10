package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/adaptive"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func read(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 16<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("single JSON object required")
	}
	return nil
}

func run() error {
	mode := flag.String("mode", "replay", "replay, shadow, tokenbar-inspect or serve")
	input := flag.String("input", "examples/adaptive/replay.json", "scenario or TokenBar JSON file")
	configPath := flag.String("config", "", "dedicated CPA YAML for serve")
	policyPath := flag.String("policy", "", "adaptive policy JSON for serve")
	statePath := flag.String("state", "", "dedicated durable adaptive state JSON for serve")
	execute := flag.Bool("enable-execute", false, "enable actual API calls on /adaptive/execute")
	flag.Parse()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	switch *mode {
	case "tokenbar-inspect":
		raw, err := os.ReadFile(*input)
		if err != nil {
			return err
		}
		result, err := adaptive.InspectTokenBar(raw)
		if err != nil {
			return err
		}
		return enc.Encode(result)
	case "replay", "shadow":
		var scenario adaptive.Scenario
		if err := read(*input, &scenario); err != nil {
			return err
		}
		if *mode == "shadow" {
			e, err := adaptive.New(scenario.Config, scenario.State)
			if err != nil {
				return err
			}
			for _, event := range scenario.Events {
				if event.Observation != nil {
					if err = e.Observe(*event.Observation); err != nil {
						return err
					}
				}
				if event.Task != nil {
					if err = enc.Encode(e.Decide(*event.Task, event.At, "adaptive", nil)); err != nil {
						return err
					}
				}
			}
			return nil
		}
		results := []adaptive.ReplayResult{}
		for _, policy := range []string{"fixed", "adaptive"} {
			result, err := adaptive.Replay(scenario, policy)
			if err != nil {
				return err
			}
			results = append(results, result)
		}
		return enc.Encode(results)
	case "serve":
		if *configPath == "" || *policyPath == "" || *statePath == "" {
			return fmt.Errorf("serve requires -config, -policy and -state")
		}
		cfg, err := config.LoadConfig(*configPath)
		if err != nil {
			return err
		}
		if cfg.Host != "127.0.0.1" && cfg.Host != "::1" {
			return fmt.Errorf("adaptive v1 server requires an explicit loopback host")
		}
		if len(cfg.APIKeys) == 0 || cfg.Plugins.Enabled || cfg.Home.Enabled {
			return fmt.Errorf("dedicated API keys required; plugins and Home must be disabled")
		}
		var policy adaptive.Config
		if err = read(*policyPath, &policy); err != nil {
			return err
		}
		var state adaptive.State
		if err = read(*statePath, &state); err != nil {
			return err
		}
		engine, err := adaptive.New(policy, state)
		if err != nil {
			return err
		}
		lock, err := os.OpenFile(*statePath+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("state locked: %w", err)
		}
		if _, err = fmt.Fprintf(lock, "%d\n", os.Getpid()); err != nil {
			lock.Close()
			return err
		}
		lock.Close()
		defer os.Remove(*statePath + ".lock")
		cfg.AuthDir = *statePath + ".auth"
		if err = os.MkdirAll(cfg.AuthDir, 0700); err != nil {
			return err
		}
		entries, err := os.ReadDir(cfg.AuthDir)
		if err != nil || len(entries) > 0 {
			return fmt.Errorf("dedicated auth directory must be empty")
		}
		cfg.AuthDir, err = filepath.Abs(cfg.AuthDir)
		if err != nil {
			return err
		}
		manager := access.NewManager()
		router := &adaptive.HTTP{Engine: engine, StatePath: *statePath, ExecuteEnabled: *execute}
		usage.RegisterNamedPlugin("adaptive-observer", adaptive.UsageObserver{})
		service, err := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(*configPath).WithRequestAccessManager(manager).WithServerOptions(
			api.WithMiddleware(func(c *gin.Context) {
				if !strings.HasPrefix(c.Request.URL.Path, "/adaptive/") {
					c.AbortWithStatus(404)
					return
				}
				c.Next()
			}),
			api.WithRouterConfigurator(func(g *gin.Engine, b *handlers.BaseAPIHandler, _ *internalconfig.Config) {
				router.Base = b
				group := g.Group("/adaptive", api.AuthMiddleware(manager))
				router.Routes(group)
			}),
		).Build()
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		err = service.Run(ctx)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	default:
		return fmt.Errorf("unknown mode")
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
