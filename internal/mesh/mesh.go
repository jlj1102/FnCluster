package mesh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fncluster/internal/cluster"
)

// Config is private cluster transport state. Cluster CA signing material exists only on its creator.
type Config struct {
	ClusterID    string               `json:"clusterId"`
	ControllerID string               `json:"controllerId"`
	NodeID       string               `json:"nodeId"`
	CA           string               `json:"ca"`
	Cert         string               `json:"cert"`
	Key          string               `json:"key"`
	CAKey        string               `json:"caKey,omitempty"`
	Members      map[string]string    `json:"members"`
	Revoked      map[string]time.Time `json:"revoked,omitempty"`
	TokenHash    string               `json:"tokenHash,omitempty"`
	TokenUntil   time.Time            `json:"tokenUntil,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	path string
	cfg  Config
}

func Open(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "mesh.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if !validID(cfg.ClusterID) || !validID(cfg.ControllerID) || !validID(cfg.NodeID) || cfg.CA == "" || cfg.Cert == "" || cfg.Key == "" || cfg.Members[cfg.ControllerID] == "" {
		return nil, errors.New("invalid mesh state")
	}
	if cfg.Members[cfg.NodeID] == "" && cfg.Revoked[cfg.NodeID].IsZero() {
		return nil, errors.New("invalid mesh state")
	}
	return &Store{path: path, cfg: cfg}, nil
}

func (s *Store) Snapshot() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	// ponytail: re-read the small state file so CLI changes (token, removal)
	// reach a running service without IPC; switch to inotify if it ever matters.
	_ = s.reloadLocked()
	cfg := s.cfg
	cfg.Members = copyMembers(cfg.Members)
	cfg.Revoked = copyTimes(cfg.Revoked)
	return cfg
}

// RevokedLocal reports whether the trust root removed this node.
func (s *Store) RevokedLocal() bool {
	cfg := s.Snapshot()
	_, revoked := cfg.Revoked[cfg.NodeID]
	return revoked
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".mesh-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func (s *Store) reloadLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if cfg.ClusterID != s.cfg.ClusterID || cfg.NodeID != s.cfg.NodeID {
		return errors.New("mesh identity changed on disk")
	}
	s.cfg = cfg
	return nil
}

func saveNew(dataDir string, cfg Config) error {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dataDir, "mesh.json")
	if _, err := os.Lstat(path); err == nil {
		return errors.New("node already belongs to a cluster")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s := &Store{path: path, cfg: cfg}
	return s.saveLocked()
}

func Init(dataDir, nodeID, advertise string) (Config, error) {
	if !validID(nodeID) {
		return Config{}, errors.New("invalid node ID")
	}
	if err := validAddress(advertise); err != nil {
		return Config{}, err
	}
	caPub, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Config{}, err
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "FnCluster CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPub, caKey)
	if err != nil {
		return Config{}, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return Config{}, err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Config{}, err
	}
	certDER, err := signNode(ca, caKey, nodeID, pub)
	if err != nil {
		return Config{}, err
	}
	clusterID, err := randomID()
	if err != nil {
		return Config{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Config{}, err
	}
	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{ClusterID: clusterID, ControllerID: nodeID, NodeID: nodeID, CA: pemText("CERTIFICATE", caDER), Cert: pemText("CERTIFICATE", certDER), Key: pemText("PRIVATE KEY", keyDER), CAKey: pemText("PRIVATE KEY", caKeyDER), Members: map[string]string{nodeID: advertise}}
	return cfg, saveNew(dataDir, cfg)
}

func (s *Store) NewToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return "", err
	}
	if s.cfg.CAKey == "" {
		return "", errors.New("only the cluster creator can issue join tokens")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	hash := sha256.Sum256([]byte(token))
	s.cfg.TokenHash = hex.EncodeToString(hash[:])
	s.cfg.TokenUntil = time.Now().Add(15 * time.Minute).UTC()
	return token, s.saveLocked()
}

func (s *Store) Fingerprint() string {
	block, _ := pem.Decode([]byte(s.Snapshot().CA))
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func (s *Store) Join(token, nodeID, address string, csrDER []byte) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return Config{}, err
	}
	if !validID(nodeID) {
		return Config{}, errors.New("invalid node ID")
	}
	if err := validAddress(address); err != nil {
		return Config{}, err
	}
	if s.cfg.CAKey == "" || s.cfg.TokenHash == "" || time.Now().After(s.cfg.TokenUntil) {
		return Config{}, errors.New("join token expired or unavailable")
	}
	hash := sha256.Sum256([]byte(token))
	expected, err := hex.DecodeString(s.cfg.TokenHash)
	if err != nil || subtle.ConstantTimeCompare(hash[:], expected) != 1 {
		return Config{}, errors.New("invalid join token")
	}
	if _, exists := s.cfg.Members[nodeID]; exists {
		return Config{}, errors.New("node already joined")
	}
	if _, revoked := s.cfg.Revoked[nodeID]; revoked {
		return Config{}, errors.New("node ID is revoked")
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil || csr.Subject.CommonName != nodeID {
		return Config{}, errors.New("invalid node CSR")
	}
	caBlock, _ := pem.Decode([]byte(s.cfg.CA))
	keyBlock, _ := pem.Decode([]byte(s.cfg.CAKey))
	if caBlock == nil || keyBlock == nil {
		return Config{}, errors.New("invalid CA state")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return Config{}, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return Config{}, err
	}
	caKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return Config{}, errors.New("invalid CA key")
	}
	certDER, err := signNode(ca, caKey, nodeID, csr.PublicKey)
	if err != nil {
		return Config{}, err
	}
	s.cfg.Members[nodeID] = address
	s.cfg.TokenHash = ""
	s.cfg.TokenUntil = time.Time{}
	if err := s.saveLocked(); err != nil {
		return Config{}, err
	}
	return Config{ClusterID: s.cfg.ClusterID, ControllerID: s.cfg.ControllerID, NodeID: nodeID, CA: s.cfg.CA, Cert: pemText("CERTIFICATE", certDER), Members: copyMembers(s.cfg.Members)}, nil
}

// Remove revokes a member and is only accepted on the cluster creator, which
// owns the CA key. Revocation is permanent for that node ID.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return err
	}
	if s.cfg.CAKey == "" {
		return errors.New("only the cluster creator can remove nodes")
	}
	if !validID(id) {
		return errors.New("invalid node ID")
	}
	if id == s.cfg.NodeID {
		return errors.New("the cluster creator cannot remove itself")
	}
	if _, ok := s.cfg.Members[id]; !ok {
		return errors.New("node is not a cluster member")
	}
	if s.cfg.Revoked == nil {
		s.cfg.Revoked = map[string]time.Time{}
	}
	s.cfg.Revoked[id] = time.Now().UTC()
	delete(s.cfg.Members, id)
	return s.saveLocked()
}

// ApplyControllerSnapshot learns member and revocation changes from the trust root.
func (s *Store) ApplyControllerSnapshot(clusterID string, members map[string]string, revoked map[string]time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return err
	}
	if clusterID != s.cfg.ClusterID {
		return errors.New("cluster ID mismatch")
	}
	if s.cfg.Revoked == nil {
		s.cfg.Revoked = map[string]time.Time{}
	}
	changed := false
	for id, at := range revoked {
		if !validID(id) {
			continue
		}
		if _, exists := s.cfg.Revoked[id]; !exists {
			s.cfg.Revoked[id] = at
			changed = true
		}
		if _, exists := s.cfg.Members[id]; exists {
			delete(s.cfg.Members, id)
			changed = true
		}
	}
	for id, address := range members {
		if !validID(id) || validAddress(address) != nil || id == s.cfg.NodeID {
			continue
		}
		if _, revokedID := s.cfg.Revoked[id]; revokedID {
			continue
		}
		if s.cfg.Members[id] != address {
			s.cfg.Members[id] = address
			changed = true
		}
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" || strings.ContainsAny(host, "/\\@") {
		return errors.New("advertise address must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("advertise port must be 1-65535")
	}
	return nil
}

func copyMembers(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for id, address := range in {
		out[id] = address
	}
	return out
}

func copyTimes(in map[string]time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(in))
	for id, at := range in {
		out[id] = at
	}
	return out
}

func randomID() (string, error) {
	var raw [16]byte
	_, err := rand.Read(raw[:])
	return hex.EncodeToString(raw[:]), err
}

func serial() *big.Int {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return new(big.Int).SetBytes(raw[:])
}

func signNode(ca *x509.Certificate, caKey ed25519.PrivateKey, id string, public any) ([]byte, error) {
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: id}, DNSNames: []string{id}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(5, 0, 0), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	return x509.CreateCertificate(rand.Reader, template, ca, public, caKey)
}

func pemText(kind string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

func certificate(cfg Config) (tls.Certificate, error) {
	return tls.X509KeyPair([]byte(cfg.Cert+cfg.CA), []byte(cfg.Key))
}

func caPool(cfg Config) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cfg.CA)) {
		return nil, errors.New("invalid cluster CA")
	}
	return pool, nil
}

func peerTLS(cfg Config, expectedID string) (*tls.Config, error) {
	cert, err := certificate(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := caPool(cfg)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || cs.PeerCertificates[0].Subject.CommonName != expectedID {
			return errors.New("unexpected peer identity")
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		return err
	}}, nil
}

func Client(cfg Config, expectedID string) (*http.Client, error) {
	tlsConfig, err := peerTLS(cfg, expectedID)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, DisableKeepAlives: true}}, nil
}

// RelayClient builds the long-lived mTLS client for the browser-facing relay:
// no request timeout, keep-alives enabled, so CGI responses and WebSocket
// upgrades survive as long as the browser keeps them open.
func RelayClient(cfg Config, expectedID string) (*http.Client, error) {
	tlsConfig, err := peerTLS(cfg, expectedID)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:     tlsConfig,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 4,
	}}, nil
}

type joinRequest struct {
	Token   string `json:"token"`
	NodeID  string `json:"nodeId"`
	Address string `json:"address"`
	CSR     []byte `json:"csr"`
}

type joinResponse struct {
	Config Config `json:"config"`
}

type PeerSnapshot struct {
	ClusterID string               `json:"clusterId"`
	Members   map[string]string    `json:"members"`
	Revoked   map[string]time.Time `json:"revoked,omitempty"`
	Node      cluster.Node         `json:"node"`
}

func (s *Store) Handler(local func() cluster.Node) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/join", func(w http.ResponseWriter, r *http.Request) {
		var req joinRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid join request", http.StatusBadRequest)
			return
		}
		cfg, err := s.Join(req.Token, req.NodeID, req.Address, req.CSR)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(joinResponse{Config: cfg})
	})
	mux.HandleFunc("GET /v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		id := peerID(r)
		if id == "" {
			http.Error(w, "mTLS member required", http.StatusForbidden)
			return
		}
		cfg := s.Snapshot()
		if _, revoked := cfg.Revoked[id]; revoked {
			// Tell the revoked node itself, so it can stop its own mesh.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(removedResponse{ClusterID: cfg.ClusterID, NodeID: id, Revoked: true})
			return
		}
		if !s.authorized(r) {
			http.Error(w, "mTLS member required", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PeerSnapshot{ClusterID: cfg.ClusterID, Members: cfg.Members, Revoked: cfg.Revoked, Node: local()})
	})
	return mux
}

type removedResponse struct {
	ClusterID string `json:"clusterId"`
	NodeID    string `json:"nodeId"`
	Revoked   bool   `json:"revoked"`
}

// peerID requires a CA-verified client certificate and returns its node ID.
func peerID(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return r.TLS.PeerCertificates[0].Subject.CommonName
}

func (s *Store) authorized(r *http.Request) bool {
	id := peerID(r)
	if id == "" {
		return false
	}
	cfg := s.Snapshot()
	if _, revoked := cfg.Revoked[id]; revoked {
		return false
	}
	_, ok := cfg.Members[id]
	return ok
}

// MarkLocalRevoked records this node's own removal and drops it from the member table.
func (s *Store) MarkLocalRevoked(clusterID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return err
	}
	if clusterID != s.cfg.ClusterID {
		return errors.New("cluster ID mismatch")
	}
	if s.cfg.Revoked == nil {
		s.cfg.Revoked = map[string]time.Time{}
	}
	s.cfg.Revoked[s.cfg.NodeID] = time.Now().UTC()
	delete(s.cfg.Members, s.cfg.NodeID)
	return s.saveLocked()
}

func (s *Store) TLSConfig() (*tls.Config, error) {
	cfg := s.Snapshot()
	cert, err := certificate(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := caPool(cfg)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}, nil
}

func JoinCluster(dataDir, nodeID, advertise, seedAddress, token, fingerprint string) error {
	if !validID(nodeID) || validAddress(advertise) != nil || validAddress(seedAddress) != nil {
		return errors.New("invalid join address or node ID")
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "mesh.json")); err == nil {
		return errors.New("node already belongs to a cluster")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: nodeID}}, private)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	fingerprint = strings.ToUpper(strings.ReplaceAll(fingerprint, ":", ""))
	if len(fingerprint) != 64 {
		return errors.New("CA fingerprint must be SHA-256 hex")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) < 2 {
			return errors.New("seed did not send cluster CA")
		}
		ca := cs.PeerCertificates[len(cs.PeerCertificates)-1]
		hash := sha256.Sum256(ca.Raw)
		if strings.ToUpper(hex.EncodeToString(hash[:])) != fingerprint {
			return errors.New("cluster CA fingerprint mismatch")
		}
		pool := x509.NewCertPool()
		pool.AddCert(ca)
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		return err
	}}}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}
	body, _ := json.Marshal(joinRequest{Token: token, NodeID: nodeID, Address: advertise, CSR: csrDER})
	response, err := client.Post("https://"+seedAddress+"/v1/join", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		reason, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		if text := strings.TrimSpace(string(reason)); text != "" {
			return fmt.Errorf("加入被拒绝：%s", text)
		}
		return fmt.Errorf("join refused: HTTP %d", response.StatusCode)
	}
	var joined joinResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&joined); err != nil {
		return err
	}
	cfg := joined.Config
	if cfg.NodeID != nodeID || !validID(cfg.ClusterID) || !validID(cfg.ControllerID) || cfg.Members[cfg.ControllerID] == "" || cfg.Members[nodeID] != advertise || cfg.CAKey != "" || cfg.Key != "" {
		return errors.New("invalid join response")
	}
	block, _ := pem.Decode([]byte(cfg.CA))
	if block == nil {
		return errors.New("invalid returned CA")
	}
	hash := sha256.Sum256(block.Bytes)
	if strings.ToUpper(hex.EncodeToString(hash[:])) != fingerprint {
		return errors.New("returned CA fingerprint mismatch")
	}
	cfg.Key = pemText("PRIVATE KEY", keyDER)
	if _, err := certificate(cfg); err != nil {
		return err
	}
	return saveNew(dataDir, cfg)
}
