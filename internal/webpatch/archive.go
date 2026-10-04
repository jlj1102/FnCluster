package webpatch

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// ArchiveBackupName is the full-copy backup of the fnOS restore archive.
const ArchiveBackupName = "webui-www-original.zip"

// ArchiveEmergencyName is an app-independent copy of the pre-cluster archive,
// placed next to www.zip so it survives even if FnCluster is removed.
const ArchiveEmergencyName = "www-before-cluster.bak"

// ensureEmergencyBackup copies the archive next to itself once, before the
// first cluster patch, so the original survives even if FnCluster is removed.
// An archive that already carries the cluster loader is not copied again.
func ensureEmergencyBackup(archivePath string) (string, error) {
	path := filepath.Join(filepath.Dir(archivePath), ArchiveEmergencyName)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if reader, err := zip.OpenReader(archivePath); err == nil {
		patched := false
		for _, file := range reader.File {
			if file.FileInfo().IsDir() || file.Name != "index.html" {
				continue
			}
			if entry, err := file.Open(); err == nil {
				data, _ := io.ReadAll(io.LimitReader(entry, 64<<10))
				entry.Close()
				if strings.Contains(string(data), BeginMarker) {
					patched = true
				}
			}
			break
		}
		reader.Close()
		if patched {
			log.Printf("webui patch: archive already carries the cluster loader; not creating %s", ArchiveEmergencyName)
			return "", nil
		}
	}
	if err := atomicCopyFile(archivePath, path, 0644); err != nil {
		return "", err
	}
	log.Printf("webui patch: emergency archive kept at %s", path)
	return path, nil
}

// archiveEntryNames lists the file entries of an archive, which works even for
// encrypted entries because the central directory stays readable.
func archiveEntryNames(reader *zip.ReadCloser) []string {
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		names = append(names, strings.TrimSuffix(file.Name, "/"))
	}
	return names
}

// rebuildPlainArchive writes an unencrypted archive from the live www tree and
// forces indexContent for index.html. fnOS accepts plaintext restore archives
// (verified on 1.2.0701), which lets encrypted originals be upgraded in place.
func rebuildPlainArchive(archivePath, wwwRoot string, indexContent []byte, required []string) error {
	for _, name := range required {
		if _, err := os.Stat(filepath.Join(wwwRoot, filepath.FromSlash(name))); err != nil {
			return fmt.Errorf("live www 缺少还原包条目 %s，拒绝重建", name)
		}
	}
	info, err := os.Lstat(archivePath)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(archivePath), ".fncluster-plain-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	writer := zip.NewWriter(temp)
	walkErr := filepath.WalkDir(wwwRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(wwwRoot, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if entry.IsDir() {
			if name == "." {
				return nil
			}
			header := &zip.FileHeader{Name: name + "/", Method: zip.Store}
			_, err := writer.CreateHeader(header)
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if _, err := os.Stat(path); err != nil {
				return nil // broken symlink: keep the original entry absent
			}
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(fileInfo)
		if err != nil {
			return err
		}
		header.Name = name
		header.Method = zip.Deflate
		target, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		if name == "index.html" && indexContent != nil {
			_, err = target.Write(indexContent)
			return err
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(target, source)
		closeErr := source.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if walkErr != nil {
		temp.Close()
		return walkErr
	}
	if err := writer.Close(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	check, err := zip.OpenReader(tempName)
	if err != nil {
		return fmt.Errorf("校验重建归档失败: %w", err)
	}
	if indexContent != nil {
		found := false
		for _, file := range check.File {
			if file.Name != "index.html" || file.FileInfo().IsDir() {
				continue
			}
			contentHash, err := hashZipEntry(file)
			if err != nil {
				check.Close()
				return err
			}
			if contentHash != hash(indexContent) {
				check.Close()
				return errors.New("重建归档的 index.html 校验失败")
			}
			found = true
			break
		}
		if !found {
			check.Close()
			return errors.New("重建归档缺少 index.html")
		}
	}
	// Close before the swap; Windows refuses to replace an open file.
	if err := check.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, archivePath)
}

// ensureArchiveBackup keeps one full copy of the original archive and refuses
// to overwrite an existing backup that does not match the current archive.
func ensureArchiveBackup(archivePath, backupPath, expectedHash string) error {
	if _, err := os.Stat(backupPath); err == nil {
		existing, err := hashFile(backupPath)
		if err != nil {
			return err
		}
		if existing != expectedHash {
			return errors.New("已有还原包备份与当前还原包不一致，拒绝继续")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicCopyFile(archivePath, backupPath, 0600)
}

func findArchiveEntry(archivePath, matchHash string) (string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("打开还原包失败: %w", err)
	}
	defer reader.Close()
	matchHash = strings.ToUpper(matchHash)
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		contentHash, err := hashZipEntry(file)
		if err != nil {
			return "", err
		}
		if contentHash == matchHash {
			return file.Name, nil
		}
	}
	return "", errors.New("还原包中找不到与当前 index.html 一致的条目")
}

// rewriteArchive atomically replaces one entry, raw-copying every other entry,
// then verifies the result before swapping it in.
func rewriteArchive(archivePath, entryName string, replacement []byte) error {
	info, err := os.Lstat(archivePath)
	if err != nil {
		return err
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("打开还原包失败: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(archivePath), ".fncluster-www-*")
	if err != nil {
		reader.Close()
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	headers, err := writeArchive(temp, reader, entryName, replacement)
	if err != nil {
		reader.Close()
		temp.Close()
		return err
	}
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		reader.Close()
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		reader.Close()
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		reader.Close()
		return err
	}
	if err := verifyLocalHeaders(tempName, headers); err != nil {
		reader.Close()
		return err
	}
	if err := verifyArchiveRewrite(tempName, reader, entryName, replacement); err != nil {
		reader.Close()
		return err
	}
	// Close the source before the swap; Windows refuses to replace an open file.
	if err := reader.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, archivePath)
}

type localHeader struct {
	name string
	raw  bool
	crc  uint32
	comp uint64
	size uint64
}

func writeArchive(target *os.File, reader *zip.ReadCloser, entryName string, replacement []byte) ([]localHeader, error) {
	writer := zip.NewWriter(target)
	headers := make([]localHeader, 0, len(reader.File))
	for _, file := range reader.File {
		header := file.FileHeader
		if file.FileInfo().IsDir() {
			headers = append(headers, localHeader{name: file.Name})
			if _, err := writer.CreateHeader(&header); err != nil {
				return nil, err
			}
			continue
		}
		if file.Name == entryName {
			rawHeader, compressed, err := buildRawReplacement(file, replacement)
			if err != nil {
				return nil, err
			}
			headers = append(headers, localHeader{name: file.Name, raw: true, crc: rawHeader.CRC32, comp: rawHeader.CompressedSize64, size: rawHeader.UncompressedSize64})
			entry, err := writer.CreateRaw(&rawHeader)
			if err != nil {
				return nil, err
			}
			if _, err := entry.Write(compressed); err != nil {
				return nil, err
			}
			continue
		}
		// CreateRaw copies the header verbatim; a preserved data-descriptor flag
		// would zero the local sizes without writing a descriptor. Go wrote the
		// real CRC and sizes into the header fields, so clear the flag.
		header.Flags &^= 0x8
		headers = append(headers, localHeader{name: file.Name, raw: true, crc: header.CRC32, comp: header.CompressedSize64, size: header.UncompressedSize64})
		entry, err := writer.CreateRaw(&header)
		if err != nil {
			return nil, err
		}
		raw, err := file.OpenRaw()
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(entry, raw); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return headers, nil
}

// buildRawReplacement deflates the new content and fills a raw header whose
// sizes and CRC are written into the local header (no data descriptor).
func buildRawReplacement(file *zip.File, replacement []byte) (zip.FileHeader, []byte, error) {
	header := file.FileHeader
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		return header, nil, err
	}
	if _, err := writer.Write(replacement); err != nil {
		return header, nil, err
	}
	if err := writer.Close(); err != nil {
		return header, nil, err
	}
	header.Method = zip.Deflate
	header.Flags &^= 0x8
	header.CRC32 = crc32.ChecksumIEEE(replacement)
	header.CompressedSize64 = uint64(compressed.Len())
	header.UncompressedSize64 = uint64(len(replacement))
	header.CompressedSize = uint32(compressed.Len())
	header.UncompressedSize = uint32(len(replacement))
	return header, compressed.Bytes(), nil
}

// verifyLocalHeaders walks the rewritten archive sequentially and rejects a
// data-descriptor flag or a CRC/size mismatch, keeping it valid for strict
// readers such as Info-ZIP. Go's writer buffers internally, so offsets are
// derived from the sequential local headers instead of tracked while writing.
func verifyLocalHeaders(path string, headers []localHeader) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	offset := int64(0)
	buf := make([]byte, 30)
	for _, header := range headers {
		if _, err := file.ReadAt(buf, offset); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(buf[0:4]) != 0x04034b50 {
			return fmt.Errorf("还原包条目 %s 本地头签名错误", header.name)
		}
		flags := binary.LittleEndian.Uint16(buf[6:8])
		if flags&0x8 != 0 {
			return fmt.Errorf("还原包条目 %s 本地头仍带数据描述符标志", header.name)
		}
		nameLen := int(binary.LittleEndian.Uint16(buf[26:28]))
		extraLen := int(binary.LittleEndian.Uint16(buf[28:30]))
		name := make([]byte, nameLen)
		if _, err := file.ReadAt(name, offset+30); err != nil {
			return err
		}
		if string(name) != header.name {
			return fmt.Errorf("还原包条目顺序错乱：期望 %s，实际 %s", header.name, name)
		}
		comp := uint64(binary.LittleEndian.Uint32(buf[18:22]))
		if header.raw {
			if crc := binary.LittleEndian.Uint32(buf[14:18]); crc != header.crc {
				return fmt.Errorf("还原包条目 %s 本地 CRC 不一致", header.name)
			}
			if comp == 0xFFFFFFFF {
				return fmt.Errorf("还原包条目 %s 使用了 ZIP64，暂不支持", header.name)
			}
			if header.comp <= 0xFFFFFFFF && comp != header.comp {
				return fmt.Errorf("还原包条目 %s 本地压缩大小不一致", header.name)
			}
			if header.size <= 0xFFFFFFFF && uint64(binary.LittleEndian.Uint32(buf[22:26])) != header.size {
				return fmt.Errorf("还原包条目 %s 本地原始大小不一致", header.name)
			}
		} else {
			comp = 0
		}
		offset += int64(30+nameLen+extraLen) + int64(comp)
	}
	return nil
}

func verifyArchiveRewrite(path string, original *zip.ReadCloser, entryName string, replacement []byte) error {
	check, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("校验新还原包失败: %w", err)
	}
	defer check.Close()
	if len(check.File) != len(original.File) {
		return errors.New("还原包条目数量不一致")
	}
	byName := make(map[string]*zip.File, len(check.File))
	for _, file := range check.File {
		byName[file.Name] = file
	}
	wanted := hash(replacement)
	for _, file := range original.File {
		rewritten, ok := byName[file.Name]
		if !ok {
			return fmt.Errorf("还原包缺少条目 %s", file.Name)
		}
		if file.Name == entryName {
			got, err := hashZipEntry(rewritten)
			if err != nil {
				return err
			}
			if got != wanted {
				return errors.New("还原包 index.html 内容校验失败")
			}
			continue
		}
		if rewritten.CRC32 != file.CRC32 || rewritten.UncompressedSize64 != file.UncompressedSize64 {
			return fmt.Errorf("还原包条目 %s 校验失败", file.Name)
		}
	}
	return nil
}

func hashZipEntry(file *zip.File) (string, error) {
	reader, err := file.Open()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, reader); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(sum.Sum(nil))), nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(sum.Sum(nil))), nil
}

func atomicCopyFile(source, target string, mode os.FileMode) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	temp, err := os.CreateTemp(filepath.Dir(target), ".fncluster-bak-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := io.Copy(temp, src); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, target)
}

// ensureArchiveSynced re-applies the archive change when the live entry is
// still patched but the archive was restored or replaced independently.
func ensureArchiveSynced(record *Record, stateDir string, replacement []byte) error {
	if record.Archive == "" {
		return nil
	}
	if _, err := os.Stat(record.Archive); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	current, err := hashFile(record.Archive)
	if err != nil {
		return err
	}
	if current == record.ArchivePatched {
		return nil
	}
	if record.ArchiveBackup == "" {
		record.ArchiveBackup = filepath.Join(stateDir, ArchiveBackupName)
	}
	entryName, findErr := findArchiveEntry(record.Archive, record.OriginalHash)
	if findErr == nil {
		if err := ensureArchiveBackup(record.Archive, record.ArchiveBackup, current); err != nil {
			return err
		}
		if err := rewriteArchive(record.Archive, entryName, replacement); err != nil {
			return err
		}
	} else {
		reader, openErr := zip.OpenReader(record.Archive)
		if openErr != nil {
			return openErr
		}
		required := archiveEntryNames(reader)
		reader.Close()
		if err := rebuildPlainArchive(record.Archive, filepath.Dir(record.Entry), replacement, required); err != nil {
			return err
		}
		entryName = "index.html"
	}
	patchedHash, err := hashFile(record.Archive)
	if err != nil {
		return err
	}
	record.ArchiveEntry = entryName
	record.ArchivePatched = patchedHash
	return writeRecord(*record, stateDir)
}

// restoreArchive puts the original archive back, but never overwrites an
// archive fnOS has updated since the patch was applied.
func restoreArchive(record Record) (bool, error) {
	if record.Archive == "" {
		return false, nil
	}
	current, err := hashFile(record.Archive)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if record.ArchivePatched == "" || current != record.ArchivePatched {
		if record.ArchiveBackup != "" {
			_ = os.Remove(record.ArchiveBackup)
		}
		log.Printf("webui restore: fnOS restore archive changed since the patch; leaving it untouched")
		return false, nil
	}
	if record.ArchiveBackup != "" {
		if _, err := os.Stat(record.ArchiveBackup); err == nil {
			mode := os.FileMode(record.ArchiveMode)
			if mode == 0 {
				mode = 0644
			}
			if err := atomicCopyFile(record.ArchiveBackup, record.Archive, mode); err != nil {
				return false, err
			}
			_ = os.Remove(record.ArchiveBackup)
			return true, nil
		}
	}
	if record.Backup == "" {
		return false, errors.New("还原包备份缺失且无原始文件，无法恢复")
	}
	original, err := os.ReadFile(record.Backup)
	if err != nil {
		return false, err
	}
	if err := rebuildPlainArchive(record.Archive, filepath.Dir(record.Entry), original, nil); err != nil {
		return false, err
	}
	return true, nil
}

// ArchiveState reports the archive and backups recorded by the current patch.
func ArchiveState(stateDir string) (string, bool, bool) {
	record, err := readRecord(stateDir)
	if err != nil || record.Archive == "" {
		return "", false, false
	}
	_, backupErr := os.Stat(record.ArchiveBackup)
	_, emergencyErr := os.Stat(record.ArchiveEmergency)
	return record.Archive, backupErr == nil, emergencyErr == nil
}
