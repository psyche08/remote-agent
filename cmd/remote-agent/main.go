package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/psyche08/remote-agent/internal/api"
	"github.com/psyche08/remote-agent/internal/autoupdate"
	"github.com/psyche08/remote-agent/internal/buildinfo"
	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/logupload"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
	"github.com/psyche08/remote-agent/internal/turnstatehook"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] == "hook" {
		return runHook(args[1:])
	}
	if len(args) > 0 && args[0] == "logs" {
		return runLogs(args[1:])
	}
	if len(args) > 0 && args[0] == "update" {
		return runUpdate(args[1:])
	}
	if len(args) > 0 && args[0] == "version" {
		b, _ := json.Marshal(buildinfo.Info())
		fmt.Println(string(b))
		return 0
	}
	fs := flag.NewFlagSet("remote-agent", flag.ContinueOnError)
	configPath := fs.String("config", "", "config.json path")
	listen := fs.String("listen", "", "TCP address for development, e.g. 127.0.0.1:18765")
	uds := fs.String("uds", "", "Unix socket path")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	baseDir, err := repoBaseDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfgPath, err := config.ResolvePath(*configPath, baseDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	stateDir := config.ResolveStateDir(cfg, baseDir)
	store := state.New(filepath.Join(stateDir, "data"))
	registry := provider.BuildRegistry(cfg)
	apiSrv := api.NewServer(cfg, registry, store)
	if claude, ok := registry["claude"].(interface{ StopCLIStream() }); ok {
		defer claude.StopCLIStream()
	}
	apiSrv.StartBackgroundWithAutoUpdate(*listen == "")
	defer apiSrv.StopBackground()
	srv := &http.Server{Handler: apiSrv.Handler()}

	if *uds != "" {
		return serveUnix(srv, *uds)
	}
	if *listen != "" {
		return serveTCP(srv, *listen)
	}
	if cfg.UDS != "" {
		return serveUnix(srv, cfg.UDS)
	}
	return serveTCP(srv, cfg.Host+":"+strconv.Itoa(cfg.Port))
}

func runUpdate(args []string) int {
	if len(args) == 0 || args[0] != "apply" {
		fmt.Fprintln(os.Stderr, "usage: remote-agent update apply --device id --target path [--relay-url url]")
		return 2
	}
	fs := flag.NewFlagSet("remote-agent update apply", flag.ContinueOnError)
	relayURL := fs.String("relay-url", "", "relay URL publishing the release manifest (required)")
	service := fs.String("service", "", "relay service name (default "+autoupdate.DefaultService+")")
	certDir := fs.String("cert-dir", "", "client cert directory for relay mTLS")
	certFile := fs.String("cert", "", "explicit client cert")
	keyFile := fs.String("key", "", "explicit client key")
	user := fs.String("user", "", "private-tunnel user id for cert discovery")
	device := fs.String("device", "", "device id")
	target := fs.String("target", "", "installed binary path the update replaces")
	staging := fs.String("staging", "", "download staging directory")
	statePath := fs.String("state", "", "auto-update state path")
	minInterval := fs.Duration("min-interval", autoupdate.DefaultMinInterval, "minimum interval between successful updates")
	healthUDS := fs.String("health-uds", "", "health check Unix socket")
	healthURL := fs.String("health-url", "", "health check URL")
	reason := fs.String("reason", "", "update trigger reason")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *relayURL == "" {
		fmt.Fprintln(os.Stderr, "relay URL required")
		return 2
	}
	if *device == "" {
		fmt.Fprintln(os.Stderr, "device id required")
		return 2
	}
	opts := autoupdate.Options{
		RelayURL:    *relayURL,
		Service:     *service,
		CertDir:     *certDir,
		CertFile:    *certFile,
		KeyFile:     *keyFile,
		UserID:      *user,
		DeviceID:    *device,
		TargetPath:  *target,
		StagingDir:  *staging,
		StatePath:   *statePath,
		MinInterval: *minInterval,
		HealthUDS:   *healthUDS,
		HealthURL:   *healthURL,
		Reason:      *reason,
		LogWriter:   os.Stderr,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := autoupdate.Apply(ctx, opts)
	b, _ := json.Marshal(res)
	if len(b) > 0 {
		fmt.Fprintln(os.Stdout, string(b))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type repeatedString []string

func (r *repeatedString) String() string { return fmt.Sprintf("%v", []string(*r)) }
func (r *repeatedString) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func runLogs(args []string) int {
	if len(args) == 0 || args[0] != "upload" {
		fmt.Fprintln(os.Stderr, "usage: remote-agent logs upload [--once] [--source path]")
		return 2
	}
	fs := flag.NewFlagSet("remote-agent logs upload", flag.ContinueOnError)
	relayURL := fs.String("relay-url", "", "relay URL, e.g. https://relay.example.com:8443")
	namespace := fs.String("namespace", "remocoding", "relay log namespace")
	user := fs.String("user", "", "private-tunnel user id; optional when cert-dir contains an agent cert for this device")
	device := fs.String("device", "", "device id")
	certDir := fs.String("cert-dir", "/opt/private-tunnel/certs", "directory containing user-*.crt/key or agent-*.crt/key")
	certFile := fs.String("cert", "", "explicit client cert")
	keyFile := fs.String("key", "", "explicit client key")
	statePath := fs.String("state", "/opt/private-tunnel/state/remote-agent/data/log-upload-state.json", "offset state JSON path")
	interval := fs.Duration("interval", time.Minute, "upload interval")
	maxChunk := fs.Int64("max-chunk", 1024*1024, "max bytes to send per source per upload")
	once := fs.Bool("once", false, "upload once and exit")
	var sources repeatedString
	fs.Var(&sources, "source", "source log path; repeatable")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *relayURL == "" {
		fmt.Fprintln(os.Stderr, "relay URL required")
		return 2
	}
	opts := logupload.Options{
		RelayURL:  *relayURL,
		Namespace: *namespace,
		UserID:    *user,
		DeviceID:  *device,
		CertDir:   *certDir,
		CertFile:  *certFile,
		KeyFile:   *keyFile,
		StatePath: *statePath,
		Sources:   []string(sources),
		MaxChunk:  *maxChunk,
		Interval:  *interval,
		Once:      *once,
		LogWriter: os.Stderr,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := logupload.Run(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runHook(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: remote-agent hook <turnstate|install-turnstate>")
		return 2
	}
	switch args[0] {
	case "turnstate":
		state := "idle"
		if len(args) > 1 {
			state = args[1]
		}
		turnstatehook.Run(state, os.Stdin, "")
		return 0
	case "install-turnstate":
		fs := flag.NewFlagSet("remote-agent hook install-turnstate", flag.ContinueOnError)
		settings := fs.String("settings", "", "Claude settings.json path")
		bin := fs.String("binary", "", "remote-agent binary path for hook commands")
		dir := fs.String("turnstate-dir", "", "turn-state directory")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		cfg, err := turnstatehook.Install(*settings, *bin, *dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("installed turn-state hooks")
		for _, cmd := range turnstatehook.InstalledCommands(cfg) {
			fmt.Println("  " + cmd)
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown hook command: "+args[0])
		return 2
	}
}

func repoBaseDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(cwd, "config.example.json")); err == nil {
		return cwd, nil
	}
	if _, err := os.Stat(filepath.Join(cwd, "remote-agent", "config.example.json")); err == nil {
		return filepath.Join(cwd, "remote-agent"), nil
	}
	return cwd, nil
}

func serveTCP(srv *http.Server, addr string) int {
	srv.Addr = addr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("remote-agent listening on http://%s\n", addr)
	return serveListener(srv, ln)
}

func serveUnix(srv *http.Server, path string) int {
	_ = os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(path)
	}()
	_ = os.Chmod(path, 0o600)
	fmt.Printf("remote-agent listening on unix://%s\n", path)
	return serveListener(srv, ln)
}

func serveListener(srv *http.Server, ln net.Listener) int {
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-signalCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	err := srv.Serve(ln)
	stop()
	<-shutdownDone
	if err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
