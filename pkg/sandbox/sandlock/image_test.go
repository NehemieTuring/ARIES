package sandlock

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractArchiveKeepsImageSymlinksInsideRoot(t *testing.T) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	body := []byte("rm")
	if err := writer.WriteHeader(&tar.Header{Name: "usr/bin/rm", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "bin", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "usr/bin"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "lib64", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/usr/lib64"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "usr/bin/python3", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/usr/bin/python3.11"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := extractArchive(root, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	assertLink(t, filepath.Join(root, "bin"), "usr/bin")
	assertLink(t, filepath.Join(root, "lib64"), "usr/lib64")
	assertLink(t, filepath.Join(root, "usr/bin/python3"), "python3.11")
	got, err := os.ReadFile(filepath.Join(root, "bin/rm"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "rm" {
		t.Fatalf("bin/rm = %q", got)
	}
}

func TestExtractArchiveRejectsEscapingSymlink(t *testing.T) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{Name: "escape", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "../outside"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(t.TempDir(), bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("expected an escaping symlink to be rejected")
	}
}

func TestExtractArchiveKeepsHardLink(t *testing.T) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	body := []byte("same")
	if err := writer.WriteHeader(&tar.Header{Name: "usr/bin/busybox", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "bin/sh", Mode: 0o755, Typeflag: tar.TypeLink, Linkname: "usr/bin/busybox"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := extractArchive(root, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "bin/sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "same" {
		t.Fatalf("hard link = %q", got)
	}
}

func assertLink(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s -> %q, want %q", path, got, want)
	}
}
