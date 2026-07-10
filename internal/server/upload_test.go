package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeImportPath(t *testing.T) {
	imp := "/lib/imports"
	cases := []struct{ rel, name, want string }{
		{"All Quiet/01.mp3", "01.mp3", "/lib/imports/All Quiet/01.mp3"},          // folder preserved
		{"", "loose.mp3", "/lib/imports/loose.mp3"},                             // no rel → flat
		{"../../etc/passwd", "x.mp3", "/lib/imports/etc/x.mp3"},                 // .. dropped; filename used; stays contained
		{"a/../../b/c.mp3", "c.mp3", "/lib/imports/a/b/c.mp3"},                  // .. segments dropped (stays contained)
		{"sub/", "f.mp3", "/lib/imports/sub/f.mp3"},                             // trailing slash → filename
	}
	for _, c := range cases {
		if got := safeImportPath(imp, c.rel, c.name); got != c.want {
			t.Errorf("safeImportPath(%q,%q) = %q, want %q", c.rel, c.name, got, c.want)
		}
		if !strings.HasPrefix(safeImportPath(imp, c.rel, c.name), imp+"/") {
			t.Errorf("escaped import dir for rel=%q", c.rel)
		}
	}
}

// A truncated/empty multipart part must FAIL (removing any partial file), never
// be silently reported as saved — the 0-byte-file bug.
func TestSaveUploadedFileRejectsEmpty(t *testing.T) {
	dir := t.TempDir()

	makeFH := func(content string) *multipart.FileHeader {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("files", "x.mp3")
		fw.Write([]byte(content))
		mw.Close()
		r := httptest.NewRequest("POST", "/", &buf)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.ParseMultipartForm(1 << 20)
		return r.MultipartForm.File["files"][0]
	}

	// Good write.
	good := filepath.Join(dir, "good.mp3")
	if err := saveUploadedFile(makeFH("ID3 real content bytes"), good); err != nil {
		t.Fatalf("good write failed: %v", err)
	}
	if fi, err := os.Stat(good); err != nil || fi.Size() == 0 {
		t.Error("good file not written")
	}

	// Empty part → error + NO file left behind.
	empty := filepath.Join(dir, "empty.mp3")
	if err := saveUploadedFile(makeFH(""), empty); err == nil {
		t.Error("empty upload was accepted, want an error")
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Error("a 0-byte file was left on disk after a failed write")
	}
}

// The upload handler returns non-200 when its (single) file is empty, so the UI
// marks it failed instead of "uploaded".
func TestHandleUploadEmptyFails(t *testing.T) {
	srv, _, dir := newTestServer(t)
	srv.LibraryDir = dir

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", "01.mp3")
	fw.Write(nil) // empty
	mw.WriteField("relpath", "Book/01.mp3")
	mw.Close()
	req := httptest.NewRequest("POST", "/api/upload?rescan=0", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.handleUpload(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("empty upload returned 200 (should fail loudly): %s", rec.Body.String())
	}
}
