package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/daemon"
	"github.com/vogler75/babel-gate/pkg/logger"
	"github.com/vogler75/babel-gate/pkg/providers/copilot"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server"
	"github.com/vogler75/babel-gate/pkg/tray"
	"github.com/vogler75/babel-gate/pkg/tui"
)

func main() {
	configPath := flag.String("config", "", "Path to YAML configuration file (optional; defaults to env vars)")
	port := flag.Int("port", 0, "Server port (overrides config)")
	copilotLogin := flag.Bool("copilot-login", false, "Perform interactive GitHub Copilot device registration")
	background := flag.Bool("background", false, "Run in background/daemon mode (no TUI, logs to rotating file; on Windows also detaches and shows a tray icon)")
	flag.BoolVar(background, "d", false, "Alias for -background")
	noTUI := flag.Bool("no-tui", false, "Disable text GUI and run with standard console logging")
	logFileFlag := flag.String("log-file", "", "Path to log file (default from config or logs/babelgate.log)")
	logMaxSizeFlag := flag.Int("log-max-size-mb", 0, "Max size in MB before log rotation (default: 10)")
	logMaxBackupsFlag := flag.Int("log-max-backups", -1, "Number of rotated log backups to keep (default: 5)")
	flag.Parse()

	// On Windows -background detaches from the console and puts an icon in the
	// notification area. On other platforms it deliberately stays in the
	// foreground so Docker/systemd supervision keeps working.
	trayMode := *background && tray.Supported()

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
		fmt.Printf("Credentials saved to %s\n", copilot.GetTokenFilePath())
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

	// Apply logging overrides from CLI flags
	if *logFileFlag != "" {
		cfg.Logging.File = *logFileFlag
	}
	if *logMaxSizeFlag > 0 {
		cfg.Logging.MaxSizeMB = *logMaxSizeFlag
	}
	if *logMaxBackupsFlag >= 0 {
		cfg.Logging.MaxBackups = *logMaxBackupsFlag
	}

	// Initialize rotating log file writer
	rotator, err := logger.NewRotator(logger.RotatorOptions{
		Filename:   cfg.Logging.File,
		MaxSizeMB:  cfg.Logging.MaxSizeMB,
		MaxBackups: cfg.Logging.MaxBackups,
	})
	if err != nil {
		log.Fatalf("Failed to initialize rotating log file %s: %v", cfg.Logging.File, err)
	}
	defer rotator.Close()

	// The tray build detaches into the background so the terminal is handed
	// straight back. This happens only after the config and the log file have
	// been validated above, so those errors are still reported on the console;
	// everything from here on is only visible in the log file.
	if trayMode && !daemon.IsChild() {
		pid, err := daemon.Detach()
		if err != nil {
			log.Fatalf("Failed to start in background: %v", err)
		}
		fmt.Printf("🗼 BabelGate started in background (PID %d)\n", pid)
		fmt.Printf("   Dashboard: http://localhost:%d/\n", cfg.Server.Port)
		fmt.Printf("   Log file:  %s\n", cfg.Logging.File)
		return
	}

	// Determine UI mode: TUI vs Background/Headless
	interactiveTerm := tui.IsTerminal()
	runTUI := !*background && !*noTUI && interactiveTerm

	// Some console hosts — notably the legacy Windows cmd.exe — report as a
	// terminal but cannot render the TUI's ANSI output. Fall back to plain
	// console logging there and say so once logging is wired up below.
	var hostNotice string
	if runTUI {
		if ok, host := tui.HostSupportsTUI(); !ok {
			runTUI = false
			hostNotice = fmt.Sprintf("Console host %q cannot render the text GUI; running in -no-tui mode (use PowerShell or Windows Terminal for the TUI)", host)
		}
	}

	ring := logger.NewRingBuffer(1000)

	if runTUI {
		// Route logs to ring buffer (for TUI view) and simultaneously to rotating log file
		mw := logger.NewMultiWriterWithRing(rotator, ring)
		log.SetOutput(mw)
	} else if *background {
		// Background daemon: output exclusively to rotating log file
		log.SetOutput(rotator)
		log.Printf("BabelGate running in background mode (logs: %s, port: %d)", cfg.Logging.File, cfg.Server.Port)
		// Now that logs go to a file, drop the console window that Windows
		// gives us when launched from Explorer. Done here rather than earlier
		// so config and log-file errors above are still visible on screen.
		if trayMode {
			tray.HideConsole()
		}
	} else {
		// Headless / non-tty console: output to both stdout and rotating log file
		mw := logger.NewMultiWriterWithRing(rotator, nil)
		log.SetOutput(io.MultiWriter(os.Stdout, mw))
	}

	if hostNotice != "" {
		log.Print(hostNotice)
	}

	engine, err := router.NewEngine(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize routing engine: %v", err)
	}

	// If not running TUI, print startup summary to console
	if !runTUI && !*background {
		fmt.Println("==================================================")
		fmt.Printf("   🗼 BABELGATE starting on :%d\n", cfg.Server.Port)
		fmt.Printf("   📝 Log File: %s (Max: %dMB, Backups: %d)\n", cfg.Logging.File, cfg.Logging.MaxSizeMB, cfg.Logging.MaxBackups)
		fmt.Println("==================================================")
	}

	// Health and connection check to all providers
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer checkCancel()

	providersMap := engine.GetProviders()
	if !runTUI && !*background {
		fmt.Println("● Checking Provider Connections & Discovering Models...")
	}

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
		if !runTUI && !*background {
			fmt.Printf("  ▶ [%s] %s (%s, Prio %d) | API Key: %s\n", strings.ToUpper(name), baseURL, p.Type(), prio, keyStatus)
		}

		models, err := p.ListModels(checkCtx)
		if err != nil {
			if !runTUI && !*background {
				fmt.Printf("    ❌ Connection Check Failed: %v\n", err)
			}
			log.Printf("[%s] connection check failed: %v", name, err)
		} else {
			engine.SyncProviderModels(name, models)
			if !runTUI && !*background {
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
	}

	if !runTUI && !*background {
		fmt.Println("==================================================")
	}

	srv := server.NewServer(cfg, engine)

	// Graceful shutdown channel
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	if runTUI {
		appTUI := tui.New(srv, engine, ring)
		tuiCtx, tuiCancel := context.WithCancel(context.Background())
		go func() {
			<-stop
			tuiCancel()
		}()
		_ = appTUI.Run(tuiCtx)
	} else if trayMode {
		trayCtx, trayCancel := context.WithCancel(context.Background())
		go func() {
			<-stop
			trayCancel()
		}()
		dashboardURL := fmt.Sprintf("http://localhost:%d/", cfg.Server.Port)
		log.Printf("Tray icon active (dashboard: %s)", dashboardURL)
		if err := tray.Run(trayCtx, tray.Options{
			Tooltip:      fmt.Sprintf("BabelGate - port %d", cfg.Server.Port),
			DashboardURL: dashboardURL,
			OnQuit:       trayCancel,
		}); err != nil {
			log.Printf("Tray unavailable (%v); waiting for termination signal instead", err)
			<-stop
		}
	} else {
		<-stop
	}

	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited cleanly.")
}
