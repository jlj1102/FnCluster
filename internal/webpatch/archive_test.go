package webpatch

import (
	"archive/zip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type zipEntry struct {
	name   string
	body   string
	method uint16
}

func writeTestArchive(t *testing.T, path string, entries []zipEntry) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: entry.method}
		if header.Method != zip.Store {
			header.Method = zip.Deflate
		}
		target, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := target.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func readZipEntry(t *testing.T, file *zip.File) string {
	t.Helper()
	reader, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestApplySyncsArchiveAndRestores(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "index.html")
	if err := os.WriteFile(entry, []byte(testHTML), 0644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	archive := filepath.Join(dir, "www.zip")
	writeTestArchive(t, archive, []zipEntry{
		{name: "index.html", body: testHTML, method: zip.Deflate},
		{name: "static/app.js", body: "console.log(1)", method: zip.Deflate},
		{name: "robots.txt", body: "User-agent: *", method: zip.Store},
	})
	originalArchiveHash, err := hashFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, archive, markerCheck(entry)); err != nil {
		t.Fatal(err)
	}
	live, err := os.ReadFile(entry)
	if err != nil || !strings.Contains(string(live), BeginMarker) {
		t.Fatalf("live entry not patched: %v", err)
	}
	backup := filepath.Join(stateDir, ArchiveBackupName)
	if backupHash, err := hashFile(backup); err != nil || backupHash != originalArchiveHash {
		t.Fatalf("archive backup mismatch: %v %s", err, backupHash)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 3 {
		t.Fatalf("archive entries = %d", len(reader.File))
	}
	for _, file := range reader.File {
		switch file.Name {
		case "index.html":
			if readZipEntry(t, file) != string(live) {
				t.Fatal("archive index is not synced with the live entry")
			}
		case "static/app.js":
			if readZipEntry(t, file) != "console.log(1)" {
				t.Fatal("deflated entry changed")
			}
		case "robots.txt":
			if readZipEntry(t, file) != "User-agent: *" {
				t.Fatal("stored entry changed")
			}
		}
	}
	reader.Close()
	record, err := readRecord(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if record.Archive != archive || record.ArchiveEntry != "index.html" || record.ArchiveBackup != backup {
		t.Fatalf("unexpected record: %+v", record)
	}
	if err := Restore(entry, stateDir); err != nil {
		t.Fatal(err)
	}
	if restored, err := hashFile(archive); err != nil || restored != originalArchiveHash {
		t.Fatalf("archive not restored: %v %s", err, restored)
	}
	if data, _ := os.ReadFile(entry); string(data) != testHTML {
		t.Fatal("live entry not restored")
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatal("archive backup was not removed")
	}
}

func TestRestoreSkipsArchiveChangedByFnOS(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "index.html")
	if err := os.WriteFile(entry, []byte(testHTML), 0644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	archive := filepath.Join(dir, "www.zip")
	writeTestArchive(t, archive, []zipEntry{{name: "index.html", body: testHTML, method: zip.Deflate}})
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, archive, markerCheck(entry)); err != nil {
		t.Fatal(err)
	}
	writeTestArchive(t, archive, []zipEntry{{name: "index.html", body: "new fnOS build", method: zip.Deflate}})
	updatedHash, err := hashFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(entry, stateDir); err != nil {
		t.Fatal(err)
	}
	if got, _ := hashFile(archive); got != updatedHash {
		t.Fatal("fnOS-updated archive was overwritten")
	}
	if data, _ := os.ReadFile(entry); string(data) != testHTML {
		t.Fatal("live entry not restored")
	}
	if _, err := os.Stat(filepath.Join(stateDir, ArchiveBackupName)); !os.IsNotExist(err) {
		t.Fatal("stale archive backup kept")
	}
}

// injectDataDescriptorFlag rewrites the on-disk local and central headers of
// one entry to look like a streamed zip entry (general purpose flag bit 3 with
// zero local sizes), reproducing the archives our raw copy must fix.
func injectDataDescriptorFlag(t *testing.T, path, name string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	patched := false
	for offset := 0; ; {
		idx := indexFrom(data, offset, []byte{'P', 'K', 3, 4})
		if idx < 0 {
			break
		}
		nameLen := int(binary.LittleEndian.Uint16(data[idx+26 : idx+28]))
		if string(data[idx+30:idx+30+nameLen]) == name {
			binary.LittleEndian.PutUint16(data[idx+6:idx+8], binary.LittleEndian.Uint16(data[idx+6:idx+8])|0x8)
			for i := idx + 14; i < idx+26; i++ {
				data[i] = 0
			}
			patched = true
			break
		}
		offset = idx + 4
	}
	for offset := 0; ; {
		idx := indexFrom(data, offset, []byte{'P', 'K', 1, 2})
		if idx < 0 {
			break
		}
		nameLen := int(binary.LittleEndian.Uint16(data[idx+28 : idx+30]))
		if string(data[idx+46:idx+46+nameLen]) == name {
			binary.LittleEndian.PutUint16(data[idx+8:idx+10], binary.LittleEndian.Uint16(data[idx+8:idx+10])|0x8)
			patched = true
			break
		}
		offset = idx + 4
	}
	if !patched {
		t.Fatalf("entry %s not found in archive", name)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func indexFrom(data []byte, offset int, pattern []byte) int {
	idx := -1
	for i := offset; i+len(pattern) <= len(data); i++ {
		if string(data[i:i+len(pattern)]) == string(pattern) {
			idx = i
			break
		}
	}
	return idx
}

func localHeaderFlags(t *testing.T, path, name string) uint16 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; ; {
		idx := indexFrom(data, offset, []byte{'P', 'K', 3, 4})
		if idx < 0 {
			t.Fatalf("local header for %s not found", name)
		}
		nameLen := int(binary.LittleEndian.Uint16(data[idx+26 : idx+28]))
		if string(data[idx+30:idx+30+nameLen]) == name {
			return binary.LittleEndian.Uint16(data[idx+6 : idx+8])
		}
		offset = idx + 4
	}
}

func TestApplyFixesDataDescriptorArchive(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "index.html")
	if err := os.WriteFile(entry, []byte(testHTML), 0644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	archive := filepath.Join(dir, "www.zip")
	writeTestArchive(t, archive, []zipEntry{
		{name: "index.html", body: testHTML, method: zip.Deflate},
		{name: "static/app.js", body: "console.log(1)", method: zip.Deflate},
	})
	injectDataDescriptorFlag(t, archive, "static/app.js")
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, archive, markerCheck(entry)); err != nil {
		t.Fatal(err)
	}
	if flags := localHeaderFlags(t, archive, "static/app.js"); flags&0x8 != 0 {
		t.Fatalf("raw-copied entry kept the data descriptor flag: %#x", flags)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, file := range reader.File {
		switch file.Name {
		case "index.html":
			if !strings.Contains(readZipEntry(t, file), BeginMarker) {
				t.Fatal("archive index not patched")
			}
		case "static/app.js":
			if readZipEntry(t, file) != "console.log(1)" {
				t.Fatal("raw-copied entry changed")
			}
		}
	}
}

func injectUnknownMethod(t *testing.T, path, name string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	patched := false
	for offset := 0; ; {
		idx := indexFrom(data, offset, []byte{'P', 'K', 3, 4})
		if idx < 0 {
			break
		}
		nameLen := int(binary.LittleEndian.Uint16(data[idx+26 : idx+28]))
		if string(data[idx+30:idx+30+nameLen]) == name {
			binary.LittleEndian.PutUint16(data[idx+8:idx+10], 99)
			patched = true
			break
		}
		offset = idx + 4
	}
	for offset := 0; ; {
		idx := indexFrom(data, offset, []byte{'P', 'K', 1, 2})
		if idx < 0 {
			break
		}
		nameLen := int(binary.LittleEndian.Uint16(data[idx+28 : idx+30]))
		if string(data[idx+46:idx+46+nameLen]) == name {
			binary.LittleEndian.PutUint16(data[idx+10:idx+12], 99)
			patched = true
			break
		}
		offset = idx + 4
	}
	if !patched {
		t.Fatalf("entry %s not found in archive", name)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRebuildsUnreadableArchive(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "www")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "index.html")
	if err := os.WriteFile(entry, []byte(testHTML), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("console.log(1)"), 0644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	archive := filepath.Join(dir, "www.zip")
	writeTestArchive(t, archive, []zipEntry{
		{name: "index.html", body: testHTML, method: zip.Deflate},
		{name: "app.js", body: "console.log(1)", method: zip.Deflate},
	})
	injectUnknownMethod(t, archive, "index.html")
	originalArchiveHash, err := hashFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, archive, markerCheck(entry)); err != nil {
		t.Fatal(err)
	}
	emergency := filepath.Join(dir, ArchiveEmergencyName)
	if backupHash, err := hashFile(emergency); err != nil || backupHash != originalArchiveHash {
		t.Fatalf("emergency backup mismatch: %v %s", err, backupHash)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 2 {
		t.Fatalf("rebuilt archive entries = %d", len(reader.File))
	}
	for _, file := range reader.File {
		switch file.Name {
		case "index.html":
			if !strings.Contains(readZipEntry(t, file), BeginMarker) {
				t.Fatal("rebuilt archive index not patched")
			}
		case "app.js":
			if readZipEntry(t, file) != "console.log(1)" {
				t.Fatal("rebuilt archive lost a file")
			}
		}
	}
	reader.Close()

	// Restore without the private full backup must rebuild a plaintext original.
	if err := os.Remove(filepath.Join(stateDir, ArchiveBackupName)); err != nil {
		t.Fatal(err)
	}
	if err := Restore(entry, stateDir); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(entry); string(data) != testHTML {
		t.Fatal("live entry not restored")
	}
	reader, err = zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.Name == "index.html" && readZipEntry(t, file) != testHTML {
			t.Fatal("rebuilt archive index was not restored")
		}
	}
	if _, err := os.Stat(emergency); err != nil {
		t.Fatalf("emergency backup was removed: %v", err)
	}
}

func TestApplyWithoutArchive(t *testing.T) {
	entry, stateDir := setup(t)
	missing := filepath.Join(t.TempDir(), "www.zip")
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, missing, markerCheck(entry)); err != nil {
		t.Fatal(err)
	}
	record, err := readRecord(stateDir)
	if err != nil || record.Archive != "" {
		t.Fatalf("archive should be empty: %+v %v", record, err)
	}
	if err := Restore(entry, stateDir); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRebuildsArchiveWithoutMatchingEntry(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "www")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "index.html")
	if err := os.WriteFile(entry, []byte(testHTML), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("console.log(1)"), 0644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	archive := filepath.Join(dir, "www.zip")
	writeTestArchive(t, archive, []zipEntry{
		{name: "index.html", body: "a different build", method: zip.Deflate},
		{name: "app.js", body: "console.log(1)", method: zip.Deflate},
	})
	if err := Apply(entry, stateDir, hash([]byte(testHTML)), "/app/fncluster/inject/loader.js", false, archive, markerCheck(entry)); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if len(reader.File) != 2 {
		t.Fatalf("rebuilt archive entries = %d", len(reader.File))
	}
	for _, file := range reader.File {
		switch file.Name {
		case "index.html":
			if !strings.Contains(readZipEntry(t, file), BeginMarker) {
				t.Fatal("rebuilt archive index not patched")
			}
		case "app.js":
			if readZipEntry(t, file) != "console.log(1)" {
				t.Fatal("rebuilt archive lost a file")
			}
		}
	}
}
