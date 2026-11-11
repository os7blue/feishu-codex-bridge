package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/os7blue/feishu-codex-bridge/internal/bridge"
	"github.com/os7blue/feishu-codex-bridge/internal/codex"
	"github.com/os7blue/feishu-codex-bridge/internal/commandset"
	"github.com/os7blue/feishu-codex-bridge/internal/config"
	"github.com/os7blue/feishu-codex-bridge/internal/diagnostics"
	"github.com/os7blue/feishu-codex-bridge/internal/feishu"
	"github.com/os7blue/feishu-codex-bridge/internal/feishuui"
	"github.com/os7blue/feishu-codex-bridge/internal/state"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("桥接程序退出", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	return runArgs(log, os.Args[1:], os.Stdout)
}

func runArgs(log *slog.Logger, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("feishu-bridge", flag.ContinueOnError)
	// Flag parsing errors may quote caller-supplied values. Return safe messages.
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "config.json", "Path to private configuration file")
	check := flags.Bool("check", false, "Validate configuration and executable without connecting")
	pair := flags.Bool("pair", false, "Enable one-time owner pairing for ten minutes")
	notify := flags.String("notify-owner", "", "Send a notification to the already-paired owner without starting Codex")
	sendFile := flags.String("send-file", "", "Send a workspace file to the paired owner without starting Codex")
	slash := flags.String("slash-commands", "", "Native commands: plan (offline), check (read-only), sync (create missing)")
	readyFile := flags.String("ready-file", "", "Write an exclusive, credential-free readiness receipt after the connection opens")
	interactionFile := flags.String("interaction-evidence-file", "", "Write payload-free interaction counters to a new receipt file")
	clickDemo := flags.Bool("click-demo", false, "After connection opens, queue status and isolated approval/answer demos for the paired owner")
	cardProbe := flags.Bool("card-probe", false, "Send four fixed non-actionable card samples to the paired owner and print safe numeric diagnostics; no Codex startup")
	maintenance := flags.String("maintenance", "", "Local running bridge control: status or stop (stop only when idle)")
	maintenancePID := flags.Int("maintenance-pid", 0, "Expected running bridge process ID for maintenance")
	maintenanceHold := flags.Bool("maintenance-hold", false, "Hold the existing state lock until stdin closes; no bridge or model startup")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.PrintDefaults()
			return nil
		}
		return errors.New("invalid command-line flags; use --help")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *maintenanceHold && (*maintenance != "" || *maintenancePID != 0 || *check || *pair || *notify != "" || *sendFile != "" || *slash != "" || *clickDemo || *cardProbe || *readyFile != "" || *interactionFile != "") {
		return errors.New("maintenance-hold requires a standalone invocation")
	}
	if *maintenance != "" || *maintenancePID != 0 {
		if (*maintenance != "status" && *maintenance != "stop") || *maintenancePID < 2 || *check || *pair || *notify != "" || *sendFile != "" || *slash != "" || *clickDemo || *cardProbe || *readyFile != "" || *interactionFile != "" {
			return errors.New("maintenance requires standalone status or stop and an expected process ID")
		}
	}
	if *sendFile != "" && (*check || *pair || *notify != "" || *slash != "" || *clickDemo || *cardProbe || *readyFile != "" || *interactionFile != "") {
		return errors.New("send-file requires a standalone invocation")
	}
	if *cardProbe && (*check || *pair || *notify != "" || *slash != "" || *clickDemo || *interactionFile != "" || *readyFile != "") {
		return errors.New("card-probe requires a standalone invocation")
	}
	slashSelected := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "slash-commands" {
			slashSelected = true
		}
	})
	if slashSelected {
		if *slash != "plan" && *slash != "check" && *slash != "sync" {
			return errors.New("slash-commands must be plan, check or sync")
		}
		if *check || *pair || *notify != "" || *clickDemo || *interactionFile != "" {
			return errors.New("slash-commands cannot be combined with check, pair or notify-owner")
		}
		if *slash == "plan" {
			return json.NewEncoder(output).Encode(commandset.All())
		}
	}
	if *notify != "" && (*check || *pair) {
		return errors.New("notify-owner cannot be combined with check or pair")
	}
	if *clickDemo && (*check || *pair || *notify != "") {
		return errors.New("click-demo requires normal startup")
	}
	if *interactionFile != "" && (*check || *pair || *notify != "") {
		return errors.New("interaction-evidence-file requires normal startup")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *maintenanceHold {
		return holdMaintenance(cfg.StateDir, os.Stdin, output)
	}
	if *maintenance != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		report, err := bridge.RequestMaintenance(ctx, cfg.StateDir, *maintenance, *maintenancePID)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(report)
	}
	if *sendFile != "" {
		name, data, err := bridge.ReadExport(cfg, cfg.ProjectDir, *sendFile)
		if err != nil {
			return err
		}
		chat, err := state.NotificationChat(cfg.StateDir, cfg.AppID, cfg.ProjectDir)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		key := make([]byte, 16)
		if _, err := rand.Read(key); err != nil {
			return errors.New("cannot generate file delivery ID")
		}
		_, err = feishu.New(cfg.AppID, cfg.AppSecret, log).SendFile(ctx, chat, name, data, hex.EncodeToString(key))
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]any{"file_delivered": true, "bytes": len(data), "additional_model_requests": false})
	}
	if *cardProbe {
		chat, err := state.NotificationChat(cfg.StateDir, cfg.AppID, cfg.ProjectDir)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		key := make([]byte, 16)
		if _, err := rand.Read(key); err != nil {
			return errors.New("cannot generate card check ID")
		}
		client := feishu.New(cfg.AppID, cfg.AppSecret, log)
		for _, sample := range feishuui.CardSamples() {
			_, sendErr := client.SendCardRouted(ctx, chat, sample.Card, hex.EncodeToString(key)+":"+sample.Name)
			var detail *feishu.DeliveryError
			report := map[string]any{"sample": sample.Name, "delivered": sendErr == nil}
			if errors.As(sendErr, &detail) {
				report["failure"] = detail
			}
			if err := json.NewEncoder(output).Encode(report); err != nil {
				return errors.New("cannot write card check result")
			}
			if sendErr != nil && (detail == nil || !detail.CardContentRejected()) {
				return errors.New("card check stopped after a delivery failure")
			}
		}
		return nil
	}
	if *slash != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		report, commandErr := feishu.New(cfg.AppID, cfg.AppSecret, log).SlashCommands(ctx, *slash == "sync")
		if err := json.NewEncoder(output).Encode(report); err != nil {
			return errors.New("cannot write native command report")
		}
		return commandErr
	}
	if *notify != "" {
		chat, err := state.NotificationChat(cfg.StateDir, cfg.AppID, cfg.ProjectDir)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		key := make([]byte, 16)
		if _, err := rand.Read(key); err != nil {
			return errors.New("cannot generate notification ID")
		}
		if err := feishu.New(cfg.AppID, cfg.AppSecret, log).Send(ctx, chat, *notify, hex.EncodeToString(key)); err != nil {
			return err
		}
		log.Info("已向配对账号发送通知")
		return nil
	}
	if _, err := exec.LookPath(cfg.CodexBinary); err != nil {
		return errors.New("codex_binary not found; install/sign in to Codex on the bridge host")
	}
	if *check {
		log.Info("配置格式、凭证非空检查及 Codex 路径检查通过；尚未验证联网和登录状态", "project", cfg.ProjectDir, "state_dir", cfg.StateDir)
		return nil
	}
	store, err := state.Open(cfg.StateDir, cfg.AppID, cfg.ProjectDir)
	if err != nil {
		return err
	}
	defer store.Close()
	if len(cfg.AllowedOpenIDs) == 0 && store.PairedID() == "" && !*pair {
		return errors.New("no authorized owner; use --pair once or set allowed_open_ids")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Raw Codex diagnostics may contain provider URLs or tokens. RPC errors are
	// reported separately with safe messages; inspect Codex locally for details.
	// Reserve the receipt before starting App Server. Never overwrite an existing
	// path, including private configuration or state supplied by mistake.
	d := &diagnostics.Collector{}
	var receipt *diagnostics.File
	if *interactionFile != "" {
		receipt, err = d.CreateFile(*interactionFile)
		if err != nil {
			return err
		}
	}
	rpc, err := codex.Start(ctx, cfg.CodexBinary, cfg.ProjectDir, cfg.CodexHome, io.Discard)
	if err != nil {
		return err
	}
	defer rpc.Close()
	client := feishu.New(cfg.AppID, cfg.AppSecret, log)
	manager := bridge.New(cfg, rpc, client, store, log, feishuui.Renderer{})
	if err := manager.Recover(); err != nil {
		return err
	}
	client.SetDiagnostics(manager.Diagnostics())
	if receipt != nil {
		// Use the collector shared by all workers; the reserved empty receipt
		// above intentionally predates any network activity.
		done := make(chan struct{})
		stop := make(chan struct{})
		go func() {
			defer close(done)
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					if receipt.SaveFrom(manager.Diagnostics()) != nil {
						log.Warn("无法保存交互诊断回执")
					}
					return
				case <-tick.C:
					if receipt.SaveFrom(manager.Diagnostics()) != nil {
						log.Warn("无法保存交互诊断回执")
						return
					}
				}
			}
		}()
		defer func() { close(stop); <-done }()
	}
	if *pair {
		code, err := manager.EnablePairing()
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "在十分钟内私聊机器人发送：/pair "+code)
	}
	closeMaintenance, err := manager.ServeMaintenance(cancel)
	if err != nil {
		return err
	}
	defer closeMaintenance()
	log.Info("桥接已启动，正在连接飞书", "project", cfg.ProjectDir)
	errorsCh := make(chan error, 2)
	var demoOnce sync.Once
	go func() { errorsCh <- manager.Run(ctx) }()
	go func() {
		errorsCh <- client.Start(ctx, manager.Submit, manager.HandleAction, func() {
			if *readyFile != "" && writeReady(*readyFile) != nil {
				log.Warn("无法保存连接就绪凭据")
			}
			if *clickDemo {
				demoOnce.Do(func() {
					if manager.SendClickDemo() != nil {
						log.Warn("点击测试卡片未能入队")
					}
				})
			}
		})
	}()
	err = <-errorsCh
	cancel()
	rpc.Close()
	<-errorsCh
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func holdMaintenance(dir string, input io.Reader, output io.Writer) error {
	lock, err := state.LockForMaintenance(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if json.NewEncoder(output).Encode(map[string]any{"protocol": 1, "phase": "locked", "pid": os.Getpid()}) != nil {
		return errors.New("cannot report maintenance lock")
	}
	var one [1]byte
	if n, err := input.Read(one[:]); n != 0 || err != io.EOF {
		return errors.New("maintenance lock control closed unexpectedly")
	}
	return nil
}

func writeReady(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create readiness receipt")
	}
	defer file.Close()
	if json.NewEncoder(file).Encode(map[string]any{"ready": true, "pid": os.Getpid()}) != nil {
		return errors.New("cannot write readiness receipt")
	}
	return file.Sync()
}
