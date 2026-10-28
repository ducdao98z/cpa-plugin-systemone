package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRepoFromURL(t *testing.T) {
	cases := map[string]string{
		"https://huggingface.co/EldanRing/Winnow-12B":           "EldanRing/Winnow-12B",
		"https://huggingface.co/EldanRing/Winnow-12B/tree/main": "EldanRing/Winnow-12B",
		"EldanRing/Winnow-E4B":                                  "EldanRing/Winnow-E4B",
		"https://huggingface.co/spaces/multimodalart/jev":       "",
		"https://github.com/Mapika/decider":                     "",
		"https://huggingface.co/../etc":                         "",
		"":                                                      "",
	}
	for in, want := range cases {
		if got := repoFromURL(in); got != want {
			t.Errorf("repoFromURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := nextLink(`<https://huggingface.co/api/x?cursor=abc>; rel="next"`); got != "https://huggingface.co/api/x?cursor=abc" {
		t.Errorf("nextLink = %q", got)
	}
}

// TestDownloadResumes lists a repo over two pages, then downloads it while one file
// already has a .part prefix, which must be resumed with a Range request.
func TestDownloadResumes(t *testing.T) {
	big := strings.Repeat("x", 5000)
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models/org/m/tree/main" && r.URL.Query().Get("cursor") == "":
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/models/org/m/tree/main?recursive=true&cursor=2>; rel="next"`, r.Host))
			fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":2},{"type":"directory","path":"sub"}]`)
		case r.URL.Path == "/api/models/org/m/tree/main":
			fmt.Fprintf(w, `[{"type":"file","path":"sub/w.bin","size":134,"lfs":{"size":%d}},{"type":"file","path":"../evil","size":1}]`, len(big))
		case r.URL.Path == "/org/m/resolve/main/config.json":
			_, _ = w.Write([]byte("{}"))
		case r.URL.Path == "/org/m/resolve/main/sub/w.bin":
			rng := r.Header.Get("Range")
			ranges = append(ranges, rng)
			from, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(big[from:]))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldBase := hfBase
	hfBase = srv.URL
	defer func() { hfBase = oldBase }()

	cfg := pluginConfig{ModelsDir: t.TempDir()}
	dir := repoDir(cfg, "org/m")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "w.bin.part"), []byte(big[:1000]), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := startDownload(cfg, "org/m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Files != 2 || d.Total != int64(len(big)+2) {
		t.Fatalf("files=%d total=%d", d.Files, d.Total)
	}
	var last download
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if last = downloadList()[0]; last.Status != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last.Status != "done" || last.Done != last.Total {
		t.Fatalf("download = %+v", last)
	}
	if len(ranges) != 1 || ranges[0] != "bytes=1000-" {
		t.Fatalf("ranges = %v", ranges)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "sub", "w.bin"))
	if string(got) != big {
		t.Fatalf("w.bin has %d bytes", len(got))
	}
	if m := localModels(cfg); len(m) != 1 || m[0].Repo != "org/m" || m[0].Partial || m[0].Files != 2 {
		t.Fatalf("local = %+v", m)
	}
	if err := deleteModel(cfg, "org/m"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("model folder still there: %v", err)
	}
}
