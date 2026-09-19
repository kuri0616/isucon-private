package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
)

func setupImageDir(t *testing.T) {
	t.Helper()
	// initImageDir は環境変数から publicDir を引き直すので、変数ではなく env を差し替える
	t.Setenv("ISUCONP_PUBLIC_DIR", t.TempDir())
	imageDirOK = false
	initImageDir()
	if !imageDirOK {
		t.Fatal("initImageDir did not mark the dir usable")
	}
}

func TestSaveImageFilePermsAndAtomicity(t *testing.T) {
	setupImageDir(t)

	if err := saveImageFile(42, "image/png", []byte("PNGDATA")); err != nil {
		t.Fatalf("saveImageFile: %v", err)
	}

	p := filepath.Join(imageDir, "42.png")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", p, err)
	}
	// nginx は www-data で動くので other に読み権限が無いと 403 になり、
	// try_files は 403 ではフォールバックしない = 全画像が死ぬ
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %o, want 644 (nginx/www-data must be able to read)", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(p); string(b) != "PNGDATA" {
		t.Fatalf("content = %q", b)
	}

	// 一時ファイルが残っていないこと
	entries, _ := os.ReadDir(imageDir)
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 file, got %d: %v", len(entries), entries)
	}

	// 上書きで自己修復すること (0 バイトファイルの治癒に使う)
	if err := saveImageFile(42, "image/png", []byte("NEWER")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "NEWER" {
		t.Fatalf("after overwrite content = %q", b)
	}

	if err := saveImageFile(43, "application/pdf", []byte("x")); err == nil {
		t.Fatal("expected unsupported mime to error")
	}
}

func TestSaveImageFileNoopWhenDirUnusable(t *testing.T) {
	setupImageDir(t)
	imageDirOK = false
	if err := saveImageFile(1, "image/png", []byte("x")); err != nil {
		t.Fatalf("expected silent noop, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(imageDir, "1.png")); !os.IsNotExist(err) {
		t.Fatal("wrote a file despite imageDirOK=false")
	}
}

func TestPruneImageFiles(t *testing.T) {
	setupImageDir(t)

	for _, name := range []string{"10.jpg", "10000.png", "10001.gif", "99999.jpg", ".tmp-abc", ".probe-xyz", "notanumber.jpg"} {
		if err := os.WriteFile(filepath.Join(imageDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pruneImageFiles(10000)

	got := map[string]bool{}
	entries, _ := os.ReadDir(imageDir)
	for _, e := range entries {
		got[e.Name()] = true
	}

	// id <= 10000 のシードは残す
	for _, keep := range []string{"10.jpg", "10000.png", "notanumber.jpg"} {
		if !got[keep] {
			t.Errorf("%s should have been kept", keep)
		}
	}
	// DELETE FROM posts WHERE id > 10000 に対応する分と作業ファイルは消す
	for _, gone := range []string{"10001.gif", "99999.jpg", ".tmp-abc", ".probe-xyz"} {
		if got[gone] {
			t.Errorf("%s should have been pruned", gone)
		}
	}
}

func TestGetImageServesFromDiskWithoutDB(t *testing.T) {
	setupImageDir(t)
	if err := saveImageFile(7, "image/jpeg", []byte("JPEGBYTES")); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Get("/image/{id}.{ext}", getImage)

	// db は nil。ディスクから返せていれば DB に触れないので panic しない
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/image/7.jpg", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "JPEGBYTES" {
		t.Fatalf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc == "" {
		t.Fatal("missing Cache-Control")
	}

	// 拡張子と mime の不一致はファイルを見ない -> DB へ行く (db=nil で panic)
	// ので、ここでは未対応拡張子だけ確認する
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/image/7.bmp", nil))
	if rec2.Code == http.StatusOK {
		t.Fatalf("unsupported ext should not be served, got %d", rec2.Code)
	}
}

func TestImageURLUnchangedForUnknownMime(t *testing.T) {
	// リファクタ前の挙動: 未知の mime では拡張子なし
	if got := imageURL(Post{ID: 5, Mime: "application/pdf"}); got != "/image/5" {
		t.Fatalf("imageURL = %q, want /image/5", got)
	}
	if got := imageURL(Post{ID: 5, Mime: "image/gif"}); got != "/image/5.gif" {
		t.Fatalf("imageURL = %q, want /image/5.gif", got)
	}
}
