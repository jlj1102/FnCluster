// Package admin exposes local management operations for the browser UI.
// Every operation affects only this node; nothing is forwarded to peers.
package admin

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fncluster/internal/cluster"
	"fncluster/internal/identity"
	"fncluster/internal/mesh"
	"fncluster/internal/webpatch"
)

type Admin struct {
	DataDir    string
	Entry      string
	LoaderURL  string
	LoaderFile string
	HealthURL  string
	SocketPath string
	MeshPort   string
	Archive    string
	LogFile    string
	State      *cluster.State

	// RelayBase is the browser-facing relay origin (empty disables relay);
	// RelayPort is the portal port used to build per-request relay URLs;
	// Relayable reports whether this node can serve an app through the relay;
	// ReportPortTarget registers a runtime-configured port the desktop
	// reported for an app (raw entries carry the resolved number).
	RelayBase        string
	RelayPort        string
	Relayable        func(string) bool
	ReportPortTarget func(appName string, port int, scheme string) bool

	mu sync.Mutex
}

type ArchiveStatus struct {
	Path      string `json:"path,omitempty"`
	BackedUp  bool   `json:"backedUp"`
	Emergency bool   `json:"emergency"`
}

type MeshStatus struct {
	Configured   bool                 `json:"configured"`
	ClusterID    string               `json:"clusterId,omitempty"`
	NodeID       string               `json:"nodeId,omitempty"`
	ControllerID string               `json:"controllerId,omitempty"`
	Fingerprint  string               `json:"caFingerprint,omitempty"`
	Members      map[string]string    `json:"members,omitempty"`
	Revoked      map[string]time.Time `json:"revoked,omitempty"`
	LocalRevoked bool                 `json:"localRevoked,omitempty"`
	IsController bool                 `json:"isController,omitempty"`
}

type TokenResult struct {
	Code             string `json:"code"`
	Fingerprint      string `json:"fingerprint"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

type Address struct {
	IP          string `json:"ip"`
	Recommended bool   `json:"recommended"`
}

const pairCodeSeparator = "."

func FormatPairCode(token, fingerprint string) string {
	return token + pairCodeSeparator + strings.ToUpper(strings.ReplaceAll(fingerprint, ":", ""))
}

// ParsePairCode accepts the copyable token.fingerprint pair shown by a peer.
func ParsePairCode(code string) (string, string, error) {
	code = strings.TrimSpace(code)
	token, fingerprint, found := strings.Cut(code, pairCodeSeparator)
	if !found {
		return "", "", errors.New("配对码格式不正确，应为 token.fingerprint")
	}
	fingerprint = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(fingerprint), ":", ""))
	if len(token) != 64 || len(fingerprint) != 64 {
		return "", "", errors.New("配对码长度不正确")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return "", "", errors.New("配对码包含非十六进制字符")
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return "", "", errors.New("配对码包含非十六进制字符")
	}
	return token, fingerprint, nil
}

func (a *Admin) MeshStatus() (MeshStatus, error) {
	store, err := mesh.Open(a.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		return MeshStatus{}, nil
	}
	if err != nil {
		return MeshStatus{}, err
	}
	cfg := store.Snapshot()
	_, revoked := cfg.Revoked[cfg.NodeID]
	return MeshStatus{
		Configured:   true,
		ClusterID:    cfg.ClusterID,
		NodeID:       cfg.NodeID,
		ControllerID: cfg.ControllerID,
		Fingerprint:  store.Fingerprint(),
		Members:      cfg.Members,
		Revoked:      cfg.Revoked,
		LocalRevoked: revoked,
		IsController: cfg.CAKey != "",
	}, nil
}

func (a *Admin) MeshInit(advertise string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, err := identity.LoadOrCreate(a.DataDir)
	if err != nil {
		return err
	}
	if _, err := mesh.Init(a.DataDir, id, strings.TrimSpace(advertise)); err != nil {
		return err
	}
	a.setLocalID(id)
	return nil
}

func (a *Admin) setLocalID(id string) {
	if a.State != nil {
		a.State.SetLocalID(id)
	}
}

func (a *Admin) MeshToken() (TokenResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	store, err := mesh.Open(a.DataDir)
	if err != nil {
		return TokenResult{}, err
	}
	token, err := store.NewToken()
	if err != nil {
		return TokenResult{}, err
	}
	fingerprint := store.Fingerprint()
	return TokenResult{Code: FormatPairCode(token, fingerprint), Fingerprint: fingerprint, ExpiresInSeconds: 900}, nil
}

func (a *Admin) MeshJoin(seed, code, advertise string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	token, fingerprint, err := ParsePairCode(code)
	if err != nil {
		return err
	}
	id, err := identity.LoadOrCreate(a.DataDir)
	if err != nil {
		return err
	}
	if err := mesh.JoinCluster(a.DataDir, id, strings.TrimSpace(advertise), strings.TrimSpace(seed), token, fingerprint); err != nil {
		return err
	}
	a.setLocalID(id)
	return nil
}

// ResetIdentity removes local cluster state and the node identity. It is the
// browser escape hatch for a revoked or stuck node. The controller keeps the
// old member record until it is removed there; the node rejoins with a new ID.
func (a *Admin) ResetIdentity() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.Remove(filepath.Join(a.DataDir, "mesh.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(a.DataDir, "node-id")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (a *Admin) MeshRemove(nodeID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	store, err := mesh.Open(a.DataDir)
	if err != nil {
		return err
	}
	return store.Remove(strings.TrimSpace(nodeID))
}

func (a *Admin) MeshLeave() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	store, err := mesh.Open(a.DataDir)
	if err != nil {
		return err
	}
	if store.Snapshot().CAKey != "" {
		return errors.New("集群创建者不能退出；请直接删除 mesh.json 或移除其他成员")
	}
	return os.Remove(filepath.Join(a.DataDir, "mesh.json"))
}

// Addresses lists non-loopback IPv4 addresses usable as this node's advertise host.
func (a *Admin) Addresses() []Address {
	result := make([]Address, 0)
	seen := map[string]bool{}
	interfaces, err := net.Interfaces()
	if err != nil {
		return result
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			result = append(result, Address{IP: ip.String()})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IP < result[j].IP })
	for i := range result {
		if net.ParseIP(result[i].IP).IsPrivate() {
			result[i].Recommended = true
			break
		}
	}
	if len(result) > 0 && !result[0].Recommended && !hasRecommended(result) {
		result[0].Recommended = true
	}
	return result
}

// RelayBaseURL picks the recommended LAN address for the relay origin.
func (a *Admin) RelayBaseURL(port string) string {
	addresses := a.Addresses()
	for _, address := range addresses {
		if address.Recommended {
			return "http://" + net.JoinHostPort(address.IP, port)
		}
	}
	if len(addresses) > 0 {
		return "http://" + net.JoinHostPort(addresses[0].IP, port)
	}
	return ""
}

func hasRecommended(addresses []Address) bool {
	for _, address := range addresses {
		if address.Recommended {
			return true
		}
	}
	return false
}

func (a *Admin) WebUIStatus() (string, error) {
	return webpatch.Status(a.Entry, a.DataDir)
}

func (a *Admin) WebUIApply(compatible bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.LoaderFile == "" || a.HealthURL == "" {
		return errors.New("WebUI 集成未配置：缺少注入文件或健康检查地址")
	}
	loader, err := os.ReadFile(a.LoaderFile)
	if err != nil {
		return fmt.Errorf("读取注入文件失败: %w", err)
	}
	return webpatch.Apply(a.Entry, a.DataDir, webpatch.SupportedIndexHash, a.LoaderURL, compatible, a.Archive, webpatch.CheckHTTP(a.HealthURL, a.LoaderURL, loader, a.SocketPath))
}

// WebUICompatible reports whether the entry is unregistered but structurally patchable.
func (a *Admin) WebUICompatible() (bool, error) {
	return webpatch.UnregisteredBuild(a.Entry)
}

// FnDeskStatus reports whether the third-party FnDesk WebUI patch is present.
// Both projects only insert their own script tag, but either side's restore can
// remove the other's injection, so the UI surfaces this before enabling.
type FnDeskStatus struct {
	Detected bool     `json:"detected"`
	Evidence []string `json:"evidence,omitempty"`
}

// fnDeskAppDir is a test seam; FnDesk installs as a normal FPK.
var fnDeskAppDir = "/var/apps/fndesk"

func (a *Admin) FnDesk() FnDeskStatus {
	var status FnDeskStatus
	add := func(evidence string) {
		for _, existing := range status.Evidence {
			if existing == evidence {
				return
			}
		}
		status.Evidence = append(status.Evidence, evidence)
	}
	if info, err := os.Stat(fnDeskAppDir); err == nil && info.IsDir() {
		add("app:" + fnDeskAppDir)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(a.Entry), "cqfndesk*.js")); err == nil {
		for _, match := range matches {
			add("file:" + filepath.Base(match))
		}
	}
	if data, err := os.ReadFile(a.Entry); err == nil {
		text := string(data)
		for _, marker := range []string{"cqfndesk", "FN_LOGIN_FOOTER", "FN_MUSIC_", "FN_VISIT_"} {
			if strings.Contains(text, marker) {
				add("index:" + marker)
			}
		}
	}
	status.Detected = len(status.Evidence) > 0
	return status
}

// WebUIArchive reports the fnOS restore archive recorded by the current patch.
func (a *Admin) WebUIArchive() ArchiveStatus {
	path, backedUp, emergency := webpatch.ArchiveState(a.DataDir)
	return ArchiveStatus{Path: path, BackedUp: backedUp, Emergency: emergency}
}

// Logs returns the tail of the service or audit log. Missing files return an empty list.
func (a *Admin) Logs(source string, lines int) ([]string, error) {
	var path string
	switch source {
	case "", "service":
		path = a.LogFile
	case "audit":
		path = filepath.Join(a.DataDir, "audit.log")
	default:
		return nil, errors.New("未知日志来源")
	}
	if path == "" {
		return nil, errors.New("未配置日志文件")
	}
	return tailFile(path, lines)
}

const maxLogBytes = 256 << 10

func tailFile(path string, lines int) ([]string, error) {
	if lines < 1 {
		lines = 200
	}
	if lines > 500 {
		lines = 500
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxLogBytes {
		if _, err := file.Seek(info.Size()-maxLogBytes, io.SeekStart); err != nil {
			return nil, err
		}
	}
	scanner := bufio.NewScanner(io.LimitReader(file, maxLogBytes))
	buffer := make([]string, 0, lines)
	for scanner.Scan() {
		buffer = append(buffer, scanner.Text())
		if len(buffer) > lines {
			buffer = buffer[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return buffer, nil
}

func (a *Admin) WebUIRestore() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return webpatch.Restore(a.Entry, a.DataDir)
}

func (a *Admin) WebUIEnable() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return webpatch.Enable(a.Entry, a.DataDir, webpatch.SupportedIndexHash)
}

// Audit appends one JSON line per privileged action. Never pass tokens or codes.
func (a *Admin) Audit(action, target, result string) {
	line, err := json.Marshal(map[string]string{
		"time":   time.Now().UTC().Format(time.RFC3339),
		"action": action,
		"target": target,
		"result": result,
	})
	if err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(a.DataDir, "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
}
