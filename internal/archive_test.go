package internal

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeArchive(t *testing.T, headers ...*tar.Header) string {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, header := range headers {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			if _, err := io.CopyN(tw, strings.NewReader(strings.Repeat("x", int(header.Size))), header.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "backup.tar.gz")
	if err := os.WriteFile(name, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func regular(name string) *tar.Header {
	return &tar.Header{Name: name, Mode: 0640, Size: 8, Typeflag: tar.TypeReg}
}

func TestRestoreRejectsUnsafeArchives(t *testing.T) {
	for _, test := range []struct {
		name   string
		header *tar.Header
	}{
		{"traversal", regular("../outside")},
		{"nested traversal", regular("dir/../../outside")},
		{"absolute", regular("/outside")},
		{"windows path", regular(`..\outside`)},
		{"symlink", &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside"}},
		{"hardlink", &tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "../outside"}},
		{"fifo", &tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(root, "outside")
			if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			data := filepath.Join(root, "data")
			if err := os.Mkdir(data, 0700); err != nil {
				t.Fatal(err)
			}
			restore := Restore{DataDirectory: data, NewDirectoryPermissions: 0755}
			if err := restore.restoreBackup(writeArchive(t, test.header)); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			content, err := os.ReadFile(outside)
			if err != nil || string(content) != "untouched" {
				t.Fatalf("outside file changed: %q %v", content, err)
			}
		})
	}
}

func TestRestoreRejectsExistingSymlinks(t *testing.T) {
	for _, directoryLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[directoryLink], func(t *testing.T) {
			outside := t.TempDir()
			data := t.TempDir()
			target := filepath.Join(outside, "secret")
			if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			linkTarget := target
			entry := "link"
			if directoryLink {
				linkTarget = outside
				entry = "link/secret"
			}
			if err := os.Symlink(linkTarget, filepath.Join(data, "link")); err != nil {
				t.Fatal(err)
			}
			restore := Restore{DataDirectory: data, NewDirectoryPermissions: 0755}
			if err := restore.restoreBackup(writeArchive(t, regular(entry))); err == nil {
				t.Fatal("existing symlink accepted")
			}
			content, err := os.ReadFile(target)
			if err != nil || string(content) != "untouched" {
				t.Fatalf("outside file changed: %q %v", content, err)
			}
		})
	}
}

func TestRestoreReplacesHardlinkWithoutChangingOtherFile(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(data, "file")); err != nil {
		t.Fatal(err)
	}
	restore := Restore{DataDirectory: data, NewDirectoryPermissions: 0755}
	if err := restore.restoreBackup(writeArchive(t, regular("file"))); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "untouched" {
		t.Fatalf("hardlink target changed: %q %v", content, err)
	}
	restored, err := os.ReadFile(filepath.Join(data, "file"))
	if err != nil || string(restored) != "xxxxxxxx" {
		t.Fatalf("replacement missing: %q %v", restored, err)
	}
}

func TestBackupRefusesSymlinks(t *testing.T) {
	for _, marker := range []bool{false, true} {
		t.Run(map[bool]string{false: "data file", true: "backup marker"}[marker], func(t *testing.T) {
			data := t.TempDir()
			outside := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			name := "link"
			if marker {
				name = "BACKUP_DATE"
			}
			if err := os.Symlink(outside, filepath.Join(data, name)); err != nil {
				t.Fatal(err)
			}
			archive, err := (Backup{DataDirectory: data}).compressDirectory("2026-11-05T12:00:00Z", "hourly")
			if err == nil {
				os.Remove(archive)
				t.Fatal("symlink accepted")
			}
			if archive != "" {
				t.Fatal("failed backup returned an archive")
			}
		})
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "nested", "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "nested", "file"), []byte("backup data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "BACKUP_DATE"), []byte("old marker"), 0600); err != nil {
		t.Fatal(err)
	}
	archive, err := (Backup{DataDirectory: data}).compressDirectory("2026-11-05T12:00:00Z", "hourly")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(archive)
	restored := t.TempDir()
	if err := (Restore{DataDirectory: restored, NewDirectoryPermissions: 0755}).restoreBackup(archive); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(restored, "nested", "file"))
	if err != nil || string(content) != "backup data" {
		t.Fatalf("round trip failed: %q %v", content, err)
	}
	info, err := os.Stat(filepath.Join(restored, "nested", "file"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("file permissions not preserved: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(restored, "nested", "empty")); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(restored, "BACKUP_DATE"))
	if err != nil || string(marker) != "hourly/2026-11-05T12:00:00Z\n" {
		t.Fatalf("incorrect marker: %q %v", marker, err)
	}
	original, err := os.ReadFile(filepath.Join(data, "BACKUP_DATE"))
	if err != nil || string(original) != "old marker" {
		t.Fatalf("backup mutated source marker: %q %v", original, err)
	}
}

func TestRestoreChecksGzipChecksum(t *testing.T) {
	archive := writeArchive(t, regular("file"))
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-8] ^= 0xff
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := (Restore{DataDirectory: t.TempDir(), NewDirectoryPermissions: 0755}).restoreBackup(archive); err == nil {
		t.Fatal("corrupt gzip checksum accepted")
	}
}

func TestRestoreFailedCopyKeepsExistingFile(t *testing.T) {
	data := t.TempDir()
	target := filepath.Join(data, "file")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(data)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	restore := Restore{DataDirectory: data}
	if err := restore.restoreFile(root, "file", regular("file"), strings.NewReader("short")); err == nil {
		t.Fatal("truncated entry accepted")
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "original" {
		t.Fatalf("existing file damaged: %q %v", content, err)
	}
	entries, err := os.ReadDir(data)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v %v", entries, err)
	}
}
