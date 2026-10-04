package webpatch

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	BeginMarker = "<!-- FNCLUSTER:BEGIN -->"
	EndMarker   = "<!-- FNCLUSTER:END -->"
	// SupportedIndexHash is the unmodified fnOS 1.2.0701 index.html in the research archive.
	SupportedIndexHash = "E77CD971FF8081E76D72517266DE8A027D4CA632EBE202E8675E2BAAB471C6CE"
	// DefaultHealthAttempts and DefaultHealthInterval cover a fnOS desktop
	// restart window, where the gateway is briefly unreachable.
	DefaultHealthAttempts = 6
	DefaultHealthInterval = 5 * time.Second
)

type Record struct {
	Entry        string `json:"entry"`
	OriginalHash string `json:"originalHash"`
	PatchedHash  string `json:"patchedHash"`
	Backup       string `json:"backup"`
	Mode         uint32 `json:"mode"`

	Archive          string `json:"archive,omitempty"`
	ArchiveEntry     string `json:"archiveEntry,omitempty"`
	ArchiveBackup    string `json:"archiveBackup,omitempty"`
	ArchiveEmergency string `json:"archiveEmergency,omitempty"`
	ArchiveOriginal  string `json:"archiveOriginalHash,omitempty"`
	ArchivePatched   string `json:"archivePatchedHash,omitempty"`
	ArchiveMode      uint32 `json:"archiveMode,omitempty"`
}

func writeRecord(record Record, stateDir string) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(stateDir, "webui-patch.json"), data, 0600)
}

// Apply patches the entry. compatible allows an unregistered hash when the
// entry still matches the fnOS desktop structure; it is an explicit admin choice.
// archive, when set and present, is fnOS's restore archive and is kept in sync
// so trim_file_monitor restores the patched build instead of the original.
func Apply(entry, stateDir, expectedHash, loaderURL string, compatible bool, archive string, check func() error) error {
	if check == nil {
		return errors.New("WebUI health check is required")
	}
	loader, err := url.Parse(loaderURL)
	if err != nil || loader.Path != "/app/fncluster/inject/loader.js" || loader.Host != "" || loader.Fragment != "" || strings.ContainsAny(loaderURL, "\"'<>\r\n") {
		return errors.New("loader URL must be /app/fncluster/inject/loader.js")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "webui-disabled")); err == nil {
		return errors.New("WebUI integration is in safe mode")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	info, err := os.Lstat(entry)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("WebUI entry must be a regular file")
	}
	original, err := os.ReadFile(entry)
	if err != nil {
		return err
	}
	if record, err := readRecord(stateDir); err == nil {
		if record.Entry != entry {
			return errors.New("patch state belongs to another entry")
		}
		if hash(original) == record.PatchedHash {
			if err := ensureArchiveSynced(&record, stateDir, original); err != nil {
				return err
			}
			return verify(entry, stateDir, check)
		}
		if hash(original) != record.OriginalHash {
			return errors.New("WebUI entry changed since backup; refusing patch")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !strings.EqualFold(hash(original), expectedHash) {
		if !compatible {
			return errors.New("unsupported WebUI entry hash")
		}
		if err := structureCompatible(original); err != nil {
			return err
		}
	}
	if strings.Contains(string(original), BeginMarker) || strings.Contains(string(original), EndMarker) {
		return errors.New("untracked FnCluster marker in WebUI entry")
	}
	index := strings.Index(string(original), "</head>")
	if index < 0 {
		return errors.New("WebUI entry has no closing head tag")
	}
	patch := BeginMarker + "\n<script src=\"" + loaderURL + "\"></script>\n" + EndMarker + "\n"
	patched := append(append([]byte{}, original[:index]...), append([]byte(patch), original[index:]...)...)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	backup := filepath.Join(stateDir, "webui-original.html")
	if data, err := os.ReadFile(backup); err == nil {
		if hash(data) != hash(original) {
			return errors.New("existing WebUI backup does not match entry")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := writeExclusive(backup, original, 0600); err != nil {
			return err
		}
	} else {
		return err
	}
	record := Record{Entry: entry, OriginalHash: hash(original), PatchedHash: hash(patched), Backup: backup, Mode: uint32(info.Mode().Perm())}
	if archive != "" {
		if archiveInfo, statErr := os.Lstat(archive); statErr == nil {
			if !archiveInfo.Mode().IsRegular() {
				return errors.New("还原包必须是普通文件")
			}
			emergency, err := ensureEmergencyBackup(archive)
			if err != nil {
				return err
			}
			originalArchiveHash, err := hashFile(archive)
			if err != nil {
				return err
			}
			archiveBackup := filepath.Join(stateDir, ArchiveBackupName)
			if err := ensureArchiveBackup(archive, archiveBackup, originalArchiveHash); err != nil {
				return err
			}
			entryName, findErr := findArchiveEntry(archive, hash(original))
			var patchedArchiveHash string
			if findErr == nil {
				if err := rewriteArchive(archive, entryName, patched); err != nil {
					return err
				}
			} else {
				// Encrypted or otherwise unreadable entry: fnOS accepts plaintext
				// restore archives, so rebuild one from the live www tree.
				reader, openErr := zip.OpenReader(archive)
				if openErr != nil {
					return fmt.Errorf("还原包不可读且无法重建: %w", openErr)
				}
				required := archiveEntryNames(reader)
				reader.Close()
				if err := rebuildPlainArchive(archive, filepath.Dir(entry), patched, required); err != nil {
					return err
				}
				entryName = "index.html"
			}
			patchedArchiveHash, err = hashFile(archive)
			if err != nil {
				return err
			}
			record.Archive = archive
			record.ArchiveEntry = entryName
			record.ArchiveBackup = archiveBackup
			record.ArchiveEmergency = emergency
			record.ArchiveOriginal = originalArchiveHash
			record.ArchivePatched = patchedArchiveHash
			record.ArchiveMode = uint32(archiveInfo.Mode().Perm())
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		} else {
			log.Printf("webui patch: fnOS restore archive %s not found; syncing the live entry only", archive)
		}
	}
	if err := writeRecord(record, stateDir); err != nil {
		if record.ArchivePatched != "" {
			_, _ = restoreArchive(record)
		}
		return err
	}
	if err := installFile(entry, patched, info.Mode().Perm(), stateDir); err != nil {
		if record.ArchivePatched == "" {
			return err
		}
		// fnOS's monitor can briefly remove the entry while restoring from the
		// archive we just synced; the health check below will see the patched copy.
		log.Printf("webui patch: live entry write failed (%v); relying on the synced archive", err)
	}
	return verify(entry, stateDir, check)
}

func verify(entry, stateDir string, check func() error) error {
	if err := check(); err != nil {
		if restoreErr := Restore(entry, stateDir); restoreErr != nil {
			return fmt.Errorf("health check failed: %v; rollback failed: %w", err, restoreErr)
		}
		return fmt.Errorf("health check failed; original WebUI restored: %w", err)
	}
	return nil
}

// Restore disables later patching and never overwrites an entry modified by fnOS.
// The fnOS restore archive is put back before the live entry so the monitor
// cannot reapply the patch afterwards.
func Restore(entry, stateDir string) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(stateDir, "webui-disabled"), []byte("safe mode\n"), 0600); err != nil {
		return err
	}
	record, err := readRecord(stateDir)
	if err != nil {
		return err
	}
	if record.Entry != entry {
		return errors.New("patch state belongs to another entry")
	}
	var archiveErr error
	if _, err := restoreArchive(record); err != nil {
		archiveErr = fmt.Errorf("还原包恢复失败: %w", err)
	}
	if err := restoreLive(entry, record, stateDir); err != nil {
		return err
	}
	return archiveErr
}

func restoreLive(entry string, record Record, stagingDir string) error {
	current, err := os.ReadFile(entry)
	if err != nil {
		return err
	}
	if hash(current) == record.OriginalHash {
		return nil
	}
	if hash(current) == record.PatchedHash {
		backup, err := os.ReadFile(record.Backup)
		if err != nil {
			return err
		}
		if hash(backup) != record.OriginalHash {
			return errors.New("WebUI backup hash mismatch")
		}
		return installFile(entry, backup, os.FileMode(record.Mode), stagingDir)
	}
	// A fnOS update may have changed the file around our loader. Remove only our marked block.
	text := string(current)
	if strings.Count(text, BeginMarker) != 1 || strings.Count(text, EndMarker) != 1 {
		return errors.New("WebUI changed and the loader marker is not unique; manual recovery needed")
	}
	start := strings.Index(text, BeginMarker)
	end := strings.Index(text, EndMarker)
	if end < start {
		return errors.New("WebUI loader marker order is invalid")
	}
	end += len(EndMarker)
	if end < len(text) && text[end] == '\n' {
		end++
	}
	info, err := os.Lstat(entry)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("WebUI entry must be a regular file")
	}
	return installFile(entry, []byte(text[:start]+text[end:]), info.Mode().Perm(), stagingDir)
}

// Enable leaves the WebUI untouched; it only permits a later explicit, checked Apply.
func Enable(entry, stateDir, expectedHash string) error {
	current, err := os.ReadFile(entry)
	if err != nil {
		return err
	}
	record, recordErr := readRecord(stateDir)
	switch {
	case recordErr == nil:
		if record.Entry != entry || record.OriginalHash != hash(current) {
			return errors.New("WebUI patch record does not match current entry")
		}
		if !strings.EqualFold(hash(current), expectedHash) {
			// A previously tracked compatible build: after recovery the entry
			// is back to its untracked original. Allow the explicit re-enable
			// when it still looks like the fnOS desktop shell.
			if err := structureCompatible(current); err != nil {
				return errors.New("unsupported WebUI entry hash")
			}
		}
	case errors.Is(recordErr, os.ErrNotExist):
		if !strings.EqualFold(hash(current), expectedHash) {
			return errors.New("unsupported WebUI entry hash")
		}
	default:
		return recordErr
	}
	err = os.Remove(filepath.Join(stateDir, "webui-disabled"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// structureCompatible verifies an unregistered build still looks like the fnOS
// desktop shell (single </head>, single React root).
func structureCompatible(original []byte) error {
	text := string(original)
	if strings.Count(text, "</head>") != 1 {
		return errors.New("WebUI 结构与已知 fnOS 桌面不符：缺少唯一的 </head>")
	}
	if strings.Count(text, `<div id="root">`) != 1 {
		return errors.New(`WebUI 结构与已知 fnOS 桌面不符：缺少唯一的 <div id="root">`)
	}
	return nil
}

// UnregisteredBuild reports whether the entry is not a registered build but is
// structurally patchable after explicit compatibility confirmation.
func UnregisteredBuild(entry string) (bool, error) {
	current, err := os.ReadFile(entry)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(hash(current), SupportedIndexHash) {
		return false, nil
	}
	if strings.Contains(string(current), BeginMarker) || strings.Contains(string(current), EndMarker) {
		return false, nil
	}
	return structureCompatible(current) == nil, nil
}

func Status(entry, stateDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(stateDir, "webui-disabled")); err == nil {
		return "SAFE_MODE", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	current, err := os.ReadFile(entry)
	if err != nil {
		return "", err
	}
	record, err := readRecord(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		if strings.Contains(string(current), BeginMarker) {
			return "BROKEN", nil
		}
		if hash(current) != SupportedIndexHash {
			return "UNSUPPORTED", nil
		}
		return "UNPATCHED", nil
	}
	if err != nil {
		return "", err
	}
	if record.Entry != entry {
		return "BROKEN", nil
	}
	switch hash(current) {
	case record.PatchedHash:
		return "PATCHED", nil
	case record.OriginalHash:
		return "UNPATCHED", nil
	default:
		return "OUTDATED", nil
	}
}

func CheckHTTP(baseURL, loaderURL string, loader []byte, socketPath string) func() error {
	return CheckHTTPWithRetry(baseURL, loaderURL, loader, socketPath, DefaultHealthAttempts, DefaultHealthInterval)
}

// CheckHTTPWithRetry retries failures a fnOS desktop restart can cause.
func CheckHTTPWithRetry(baseURL, loaderURL string, loader []byte, socketPath string, attempts int, interval time.Duration) func() error {
	return func() error {
		for attempt := 1; attempt <= attempts; attempt++ {
			retryable, err := checkHTTPOnce(baseURL, loaderURL, loader, socketPath)
			if err == nil {
				return nil
			}
			if !retryable || attempt == attempts {
				return err
			}
			log.Printf("webui health: attempt %d/%d failed (%v); retrying in %s", attempt, attempts, err, interval)
			time.Sleep(interval)
		}
		return errors.New("webui health check did not run")
	}
}

// checkHTTPOnce returns retryable=true for transient failures: connection
// errors, 502/503/504, or a served entry the restarted gateway has not replaced yet.
func checkHTTPOnce(baseURL, loaderURL string, loader []byte, socketPath string) (bool, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") {
		return false, errors.New("health URL must be HTTP(S)")
	}
	ip := net.ParseIP(base.Hostname())
	if base.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false, errors.New("health URL must target local fnOS")
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	index, err := request(client, base.String())
	if err != nil {
		return true, err
	}
	if !strings.Contains(string(index.body), BeginMarker) {
		if !gatewayAuthWall(index) {
			if transientStatus(index.status) {
				return true, fmt.Errorf("health request %s returned %d", base, index.status)
			}
			if index.status == http.StatusOK {
				return true, errors.New("served WebUI entry is missing FnCluster loader")
			}
			return false, fmt.Errorf("health request %s returned %d", base, index.status)
		}
		// The fnOS gateway hides the desktop from unauthenticated clients, so
		// the public entry cannot be verified from the agent. The on-disk patch
		// was just verified by Apply; the badge must be confirmed in a logged-in browser.
		log.Printf("webui health: public entry is behind fnOS session auth; confirm the badge in an authenticated browser")
	}
	ref, err := url.Parse(loaderURL)
	if err != nil {
		return false, err
	}
	servedLoader, err := request(client, base.ResolveReference(ref).String())
	if err == nil && servedLoader.status == http.StatusOK && hash(servedLoader.body) == hash(loader) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if !gatewayAuthWall(servedLoader) {
		if transientStatus(servedLoader.status) {
			return true, fmt.Errorf("public loader route returned %d", servedLoader.status)
		}
		return false, errors.New("public loader route did not serve the expected file")
	}
	// fnOS denies unauthenticated gateway requests. Verify the same loader
	// through the local FPK socket; the browser must still be checked after apply.
	if !filepath.IsAbs(socketPath) {
		return false, errors.New("local loader socket path must be absolute")
	}
	local := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}}
	servedLoader, err = request(local, "http://fncluster"+ref.RequestURI())
	if err != nil {
		return true, err
	}
	if servedLoader.status != http.StatusOK || hash(servedLoader.body) != hash(loader) {
		// During an upgrade the socket can briefly belong to the previous
		// package's process; wait for the restart instead of rolling back.
		return true, errors.New("served loader does not match local loader file")
	}
	return false, nil
}

func transientStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

type healthResponse struct {
	status int
	body   []byte
}

func request(client *http.Client, target string) (healthResponse, error) {
	response, err := client.Get(target)
	if err != nil {
		return healthResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return healthResponse{}, err
	}
	return healthResponse{status: response.StatusCode, body: body}, nil
}

// gatewayAuthWall recognizes the responses fnOS returns to unauthenticated
// gateway clients: a redirect to login, 401/403, or the observed "invalid token" body.
func gatewayAuthWall(response healthResponse) bool {
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
		return true
	}
	if response.status >= 300 && response.status < 400 {
		return true
	}
	return strings.TrimSpace(string(response.body)) == "invalid token"
}

func readRecord(stateDir string) (Record, error) {
	var record Record
	data, err := os.ReadFile(filepath.Join(stateDir, "webui-patch.json"))
	if err != nil {
		return record, err
	}
	err = json.Unmarshal(data, &record)
	return record, err
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".fncluster-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// renameFile is a seam so tests can exercise the retry path.
var renameFile = os.Rename

// installFile replaces path with data, staging the temp file outside the target
// directory. fnOS's trim_file_monitor removes unfamiliar files under
// /usr/trim/www, which used to make an in-directory rename fail with ENOENT.
func installFile(path string, data []byte, mode os.FileMode, stagingDir string) error {
	if err := os.MkdirAll(stagingDir, 0700); err != nil {
		return err
	}
	var lastErr error
	for _, dir := range []string{stagingDir, filepath.Dir(path)} {
		temp, err := os.CreateTemp(dir, ".fncluster-install-*")
		if err != nil {
			lastErr = err
			continue
		}
		tempName := temp.Name()
		if _, err := temp.Write(data); err != nil {
			temp.Close()
			os.Remove(tempName)
			lastErr = err
			continue
		}
		if err := temp.Chmod(mode); err != nil {
			temp.Close()
			os.Remove(tempName)
			lastErr = err
			continue
		}
		if err := temp.Sync(); err != nil {
			temp.Close()
			os.Remove(tempName)
			lastErr = err
			continue
		}
		if err := temp.Close(); err != nil {
			os.Remove(tempName)
			lastErr = err
			continue
		}
		for attempt := 0; attempt < 3; attempt++ {
			lastErr = renameFile(tempName, path)
			if lastErr == nil {
				return nil
			}
			if !retryableRename(lastErr) {
				break
			}
			time.Sleep(time.Duration(100*(attempt+1)) * time.Millisecond)
		}
		os.Remove(tempName)
	}
	return lastErr
}

func retryableRename(err error) bool {
	return os.IsNotExist(err) || os.IsExist(err)
}
