package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/providers/copilot"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server"
)

func main() {
	configPath := flag.String("config", "", "Path to YAML configuration file (optional; defaults to env vars)")
	port := flag.Int("port", 0, "Server port (overrides config)")
	copilotLogin := flag.Bool("copilot-login", false, "Perform interactive GitHub Copilot device registration")
	flag.Parse()

	if *copilotLogin {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		fmt.Println("==================================================")
		fmt.Println("   🔑 GITHUB COPILOT DEVICE REGISTRATION")
		fmt.Println("==================================================")
		fmt.Println("Requesting one-time device code from GitHub...")
		dcr, err := copilot.RequestDeviceCode(ctx, nil)
		if err != nil {
			log.Fatalf("Failed to request device code: %v", err)
		}

		fmt.Println("\nTo complete authorization:")
		fmt.Printf("  1. Open your browser: %s\n", dcr.VerificationURI)
		fmt.Printf("  2. Enter device code: %s\n\n", dcr.UserCode)
		fmt.Printf("⏳ Waiting for authorization in browser (code expires in %d minutes)...\n", dcr.ExpiresIn/60)

		otr, err := copilot.PollDeviceToken(ctx, nil, dcr.DeviceCode, dcr.Interval)
		if err != nil {
			log.Fatalf("\nAuthentication failed: %v", err)
		}

		username, _ := copilot.GetAuthenticatedUser(ctx, nil, otr.AccessToken)
		if err := copilot.SaveTokenToDisk(otr.AccessToken, username); err != nil {
			log.Printf("Warning: failed to save token to disk: %v", err)
		}

		fmt.Println("\n==================================================")
		if username != "" {
			fmt.Printf("🎉 Successfully connected as @%s!\n", username)
		} else {
			fmt.Println("🎉 Successfully connected to GitHub Copilot!")
		}
		fmt.Println("Credentials saved to ~/.config/github-copilot/hosts.json")
		fmt.Println("==================================================")
		return
	}

	// If -config wasn't specified, check if config.yaml exists locally
	path := *configPath
	if path == "" {
		if _, err := os.Stat("config.yaml"); err == nil {
			path = "config.yaml"
		}
	}

	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if *port > 0 {
		cfg.Server.Port = *port
	}

	engine, err := router.NewEngine(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize routing engine: %v", err)
	}

	// Print startup summary
	providersMap := engine.GetProviders()
	fmt.Println("==================================================")
	fmt.Printf("   🗼 BABELGATE starting on :%d\n", cfg.Server.Port)
	fmt.Println("==================================================")
	// Health and connection check to all providers
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer checkCancel()

	fmt.Println("● Checking Provider Connections & Discovering Models...")
	for name, p := range providersMap {
		provCfg := cfg.Providers[name]
		keyStatus := "configured"
		if provCfg.APIKey == "" || strings.HasPrefix(provCfg.APIKey, "${") {
			keyStatus = "MISSING/EMPTY (will rely on client token passthrough)"
		} else {
			keyLen := len(provCfg.APIKey)
			if keyLen > 8 {
				keyStatus = fmt.Sprintf("set (%s...%s, len %d)", provCfg.APIKey[:4], provCfg.APIKey[keyLen-4:], keyLen)
			} else {
				keyStatus = "set (short key)"
			}
		}

		baseURL := provCfg.BaseURL
		if baseURL == "" {
			baseURL = "(default cloud API)"
		}

		prio := engine.GetProviderPriority(name)
		fmt.Printf("  ▶ [%s] %s (%s, Prio %d) | API Key: %s\n", strings.ToUpper(name), baseURL, p.Type(), prio, keyStatus)

		models, err := p.ListModels(checkCtx)
		if err != nil {
			fmt.Printf("    ❌ Connection Check Failed: %v\n", err)
		} else {
			engine.SyncProviderModels(name, models)
			fmt.Printf("    ✅ Connected successfully! (%d models available)\n", len(models))
			for i, m := range models {
				if i < 8 {
					fmt.Printf("       • %s\n", m.ID)
				} else if i == 8 {
					fmt.Printf("       • ... and %d more models\n", len(models)-8)
					break
				}
			}
		}
	}
	fmt.Println("==================================================")

	srv := server.NewServer(cfg, engine)

	// Graceful shutdown channel
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited cleanly.")
}
