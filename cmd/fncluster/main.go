package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"fncluster/internal/admin"
	"fncluster/internal/agent"
	"fncluster/internal/api"
	"fncluster/internal/identity"
	"fncluster/internal/mesh"
	"fncluster/internal/relay"
	"fncluster/internal/webpatch"
)

type paths []string

func (p *paths) String() string { return fmt.Sprint([]string(*p)) }
func (p *paths) Set(value string) error {
	*p = append(*p, value)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fncluster serve | cluster init|token|join|remove|leave|status | webui status|apply|enable | recover webui")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "cluster":
		return clusterCommand(args[1:])
	case "webui":
		return webui(args[1:])
	case "recover":
		if len(args) < 2 || args[1] != "webui" {
			return errors.New("usage: fncluster recover webui [--entry PATH] [--state-dir DIR]")
		}
		entry, stateDir, err := webuiPaths("recover webui", args[2:])
		if err != nil {
			return err
		}
		return webpatch.Restore(entry, stateDir)
	default:
		return errors.New("usage: fncluster serve | cluster init|token|join|remove|leave|status | webui status|apply|enable | recover webui")
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	dataDir := flags.String("data-dir", filepath.Join(".", "state"), "private persistent state directory")
	listen := flags.String("listen", "127.0.0.1:38670", "local HTTP listener")
	unixSocket := flags.String("unix-socket", "", "local Unix socket for the fnOS application gateway")
	meshListen := flags.String("mesh-listen", ":38671", "mTLS node listener when joined")
	injectFile := flags.String("inject-file", "", "local WebUI loader file used by browser management")
	webuiHealth := flags.String("webui-health-url", "", "local fnOS WebUI URL used by browser patch health checks")
	webuiEntry := flags.String("webui-entry", "/usr/trim/www/index.html", "fnOS WebUI entry patched by browser management")
	webuiArchive := flags.String("webui-archive", "/usr/trim/share/.restore/www.zip", "fnOS www restore archive kept in sync with the patch")
	logFile := flags.String("log-file", "", "service log file exposed read-only in the browser UI")
	relayListen := flags.String("relay-listen", ":38672", "browser-facing remote app relay portal; empty disables it")
	relayPorts := flags.Int("relay-ports", 32, "relay origin ports (one per concurrently open relayed app)")
	var mounts paths
	flags.Var(&mounts, "storage-mount", "storage mount to include in aggregate statistics; repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("HTTP API must listen on loopback until fnOS authorization is integrated")
	}
	agent, err := agent.New(*dataDir, mounts)
	if err != nil {
		return err
	}
	var listener net.Listener
	if *unixSocket != "" {
		if !filepath.IsAbs(*unixSocket) {
			return errors.New("Unix socket path must be absolute")
		}
		if info, statErr := os.Lstat(*unixSocket); statErr == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return errors.New("Unix socket path already contains a non-socket file")
			}
			if removeErr := os.Remove(*unixSocket); removeErr != nil {
				return removeErr
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		listener, err = net.Listen("unix", *unixSocket)
		if err == nil {
			// The fnOS gateway was verified to reach a root-owned 0600 socket, so
			// other local users cannot bypass the gateway to reach mutating routes.
			err = os.Chmod(*unixSocket, 0600)
			if err != nil {
				listener.Close()
				os.Remove(*unixSocket)
				return err
			}
			defer os.Remove(*unixSocket)
		}
	} else {
		listener, err = net.Listen("tcp", *listen)
	}
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go agent.Run(ctx, 5*time.Second)
	_, meshPort, err := net.SplitHostPort(*meshListen)
	if err != nil {
		return fmt.Errorf("invalid mesh-listen address: %w", err)
	}
	meshRuntime := mesh.NewRuntime(*dataDir, *meshListen, agent.State, 2*time.Second)
	go meshRuntime.Run(ctx)
	adminOps := &admin.Admin{
		DataDir:    *dataDir,
		Entry:      *webuiEntry,
		LoaderURL:  "/app/fncluster/inject/loader.js?v=1",
		LoaderFile: *injectFile,
		HealthURL:  *webuiHealth,
		SocketPath: *unixSocket,
		MeshPort:   meshPort,
		Archive:    *webuiArchive,
		LogFile:    *logFile,
		State:      agent.State,
	}
	adminOps.Relayable = agent.Relayable
	adminOps.ReportPortTarget = agent.AddReportedTarget
	var relayServer *relay.Server
	if *relayListen != "" && *relayPorts > 0 {
		_, relayPort, relayErr := net.SplitHostPort(*relayListen)
		if relayErr != nil {
			return fmt.Errorf("invalid relay-listen address: %w", relayErr)
		}
		adminOps.RelayPort = relayPort
		adminOps.RelayBase = adminOps.RelayBaseURL(relayPort)
		if adminOps.RelayBase == "" {
			log.Printf("relay disabled: no LAN address found")
		} else {
			relayServer = &relay.Server{DataDir: *dataDir, TTL: 12 * time.Hour, Sessions: relay.NewSessions(12 * time.Hour), Ports: *relayPorts, LocalIcon: agent.LocalIcon}
			meshRuntime.SetRelay(relay.RemoteHandler(agent.ResolveTarget))
			go func() {
				if relayErr := relayServer.Serve(ctx, *relayListen); relayErr != nil {
					log.Printf("relay listener: %v", relayErr)
				}
			}()
			log.Printf("FnCluster relay portal %s (+%d origins, browser origin %s)", *relayListen, *relayPorts, adminOps.RelayBase)
		}
	}
	go reapplyWebUIAfterUpgrade(*dataDir, *webuiEntry, adminOps)
	server := &http.Server{
		Handler: api.New(agent.State, adminOps, meshRuntime, relayServer), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("FnCluster listening on %s", listener.Addr())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// reapplyWebUIAfterUpgrade restores a desktop patch that upgrade_init removed.
// It runs once per FPK upgrade, marked by webui-reapply, and only after this
// process owns the Unix socket: the health check then compares the served
// loader against the file shipped with this build, so a stale process or a
// start-up race cannot roll the patch back into safe mode.
func reapplyWebUIAfterUpgrade(dataDir, entry string, adminOps *admin.Admin) {
	marker := filepath.Join(dataDir, "webui-reapply")
	if _, err := os.Stat(marker); err != nil {
		return
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("webui upgrade re-apply: %v", err)
		return
	}
	compatible, err := webpatch.UnregisteredBuild(entry)
	if err != nil {
		log.Printf("webui upgrade re-apply skipped: %v", err)
		return
	}
	if err := adminOps.WebUIEnable(); err != nil {
		log.Printf("webui upgrade re-apply: enable failed: %v", err)
		return
	}
	if err := adminOps.WebUIApply(compatible); err != nil {
		log.Printf("webui upgrade re-apply failed: %v", err)
		return
	}
	log.Printf("webui patch re-applied after upgrade (compatible=%v)", compatible)
}

func clusterCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fncluster cluster init|token|join|remove|leave|status")
	}
	flags := flag.NewFlagSet("cluster "+args[0], flag.ContinueOnError)
	dataDir := flags.String("data-dir", filepath.Join(".", "state"), "private persistent state directory")
	advertise := flags.String("advertise", "", "this node's reachable host:port")
	seed := flags.String("seed", "", "existing node host:port")
	token := flags.String("token", "", "one-time join token")
	fingerprint := flags.String("ca-fingerprint", "", "cluster CA SHA-256 fingerprint")
	node := flags.String("node", "", "member node ID to revoke")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	switch args[0] {
	case "init":
		id, err := identity.LoadOrCreate(*dataDir)
		if err != nil {
			return err
		}
		cfg, err := mesh.Init(*dataDir, id, *advertise)
		if err != nil {
			return err
		}
		joined, err := mesh.Open(*dataDir)
		if err != nil {
			return err
		}
		fmt.Printf("clusterId=%s\nnodeId=%s\ncaFingerprint=%s\n", cfg.ClusterID, cfg.NodeID, joined.Fingerprint())
		return nil
	case "token":
		joined, err := mesh.Open(*dataDir)
		if err != nil {
			return err
		}
		value, err := joined.NewToken()
		if err != nil {
			return err
		}
		fmt.Printf("token=%s\ncaFingerprint=%s\nexpiresIn=15m\n", value, joined.Fingerprint())
		return nil
	case "join":
		id, err := identity.LoadOrCreate(*dataDir)
		if err != nil {
			return err
		}
		return mesh.JoinCluster(*dataDir, id, *advertise, *seed, *token, *fingerprint)
	case "remove":
		if *node == "" {
			return errors.New("cluster remove requires --node")
		}
		joined, err := mesh.Open(*dataDir)
		if err != nil {
			return err
		}
		if err := joined.Remove(*node); err != nil {
			return err
		}
		fmt.Printf("revoked=%s\n", *node)
		return nil
	case "leave":
		joined, err := mesh.Open(*dataDir)
		if err != nil {
			return err
		}
		if joined.Snapshot().CAKey != "" {
			return errors.New("the cluster creator cannot leave; that would orphan members")
		}
		if err := os.Remove(filepath.Join(*dataDir, "mesh.json")); err != nil {
			return err
		}
		fmt.Println("left cluster; node identity kept and can join again")
		return nil
	case "status":
		joined, err := mesh.Open(*dataDir)
		if err != nil {
			return err
		}
		cfg := joined.Snapshot()
		fmt.Printf("clusterId=%s\nnodeId=%s\ncontrollerId=%s\ncaFingerprint=%s\n", cfg.ClusterID, cfg.NodeID, cfg.ControllerID, joined.Fingerprint())
		if _, revoked := cfg.Revoked[cfg.NodeID]; revoked {
			fmt.Println("localRevoked=true")
		}
		ids := make([]string, 0, len(cfg.Members))
		for id := range cfg.Members {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Printf("member=%s\t%s\n", id, cfg.Members[id])
		}
		revoked := make([]string, 0, len(cfg.Revoked))
		for id := range cfg.Revoked {
			revoked = append(revoked, id)
		}
		sort.Strings(revoked)
		for _, id := range revoked {
			fmt.Printf("revoked=%s\t%s\n", id, cfg.Revoked[id].UTC().Format(time.RFC3339))
		}
		return nil
	default:
		return errors.New("usage: fncluster cluster init|token|join|remove|leave|status")
	}
}

func webui(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fncluster webui status|apply|enable")
	}
	if args[0] == "status" {
		entry, stateDir, err := webuiPaths("webui status", args[1:])
		if err != nil {
			return err
		}
		status, err := webpatch.Status(entry, stateDir)
		if err == nil {
			fmt.Println(status)
		}
		return err
	}
	if args[0] == "enable" {
		entry, stateDir, err := webuiPaths("webui enable", args[1:])
		if err != nil {
			return err
		}
		return webpatch.Enable(entry, stateDir, webpatch.SupportedIndexHash)
	}
	if args[0] != "apply" {
		return errors.New("usage: fncluster webui status|apply|enable")
	}
	flags := flag.NewFlagSet("webui apply", flag.ContinueOnError)
	entry := flags.String("entry", "/usr/trim/www/index.html", "fnOS WebUI entry")
	stateDir := flags.String("state-dir", "/var/apps/fncluster/var", "private recovery state directory")
	loaderURL := flags.String("loader-url", "/app/fncluster/inject/loader.js?v=1", "fnOS-served loader URL")
	loaderFile := flags.String("loader-file", "", "local loader file served at loader-url")
	loaderSocket := flags.String("loader-socket", "/var/apps/fncluster/target/fncluster.sock", "local FnCluster gateway socket for unauthenticated health checks")
	healthURL := flags.String("health-url", "", "local fnOS WebUI URL for post-patch check")
	compatible := flags.Bool("compatible", false, "allow an unregistered fnOS build when the desktop structure matches")
	archive := flags.String("webui-archive", "/usr/trim/share/.restore/www.zip", "fnOS www restore archive kept in sync with the patch")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *loaderFile == "" || *healthURL == "" {
		return errors.New("webui apply requires --loader-file and --health-url, with no positional arguments")
	}
	loader, err := os.ReadFile(*loaderFile)
	if err != nil {
		return err
	}
	return webpatch.Apply(*entry, *stateDir, webpatch.SupportedIndexHash, *loaderURL, *compatible, *archive, webpatch.CheckHTTP(*healthURL, *loaderURL, loader, *loaderSocket))
}

func webuiPaths(name string, args []string) (string, string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	entry := flags.String("entry", "/usr/trim/www/index.html", "fnOS WebUI entry")
	stateDir := flags.String("state-dir", "/var/apps/fncluster/var", "private recovery state directory")
	if err := flags.Parse(args); err != nil {
		return "", "", err
	}
	if flags.NArg() != 0 {
		return "", "", errors.New("unexpected positional arguments")
	}
	return *entry, *stateDir, nil
}
