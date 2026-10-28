package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	hfIndexSpace     = "/spaces/multimodalart/jev-decision-index"
	hfIndexTTL       = 15 * time.Minute
	defaultModelsDir = "plugins/data/models"
	partSuffix       = ".part"
)

// hfBase is a variable so tests can point it at a local server.
var hfBase = "https://huggingface.co"

// hfClient talks to Hugging Face directly: downloads stream to disk, which host.http.do cannot do
// because it returns whole bodies. The default transport honours HTTPS_PROXY.
var hfClient = &http.Client{}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9_.-]+$`)

func validRepo(repo string) bool {
	return repoPattern.MatchString(repo) && !strings.Contains(repo, "..")
}

func hfGet(ctx context.Context, token, rawURL string, header http.Header) (*http.Response, error) {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if errNew != nil {
		return nil, errNew
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return hfClient.Do(req)
}

// hfJSON fetches a JSON document and returns the response headers for paging.
func hfJSON(token, rawURL string, out any) (http.Header, error) {
	resp, errGet := hfGet(context.Background(), token, rawURL, nil)
	if errGet != nil {
		return nil, errGet
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode != http.StatusOK {
		return nil, hfError(resp.StatusCode, body)
	}
	return resp.Header, json.Unmarshal(body, out)
}

// hfStatusError is a non-200 answer from Hugging Face.
type hfStatusError struct {
	Status int
	Msg    string
}

func (e *hfStatusError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "Hugging Face refused access (401). The repository may be private or gated, or the token is wrong: " + e.Msg
	case http.StatusForbidden:
		return "This model is gated (403). Accept its license on huggingface.co with the account that owns the token: " + e.Msg
	case http.StatusNotFound:
		return "Not found on Hugging Face (404): " + e.Msg
	}
	return fmt.Sprintf("Hugging Face answered %d: %s", e.Status, e.Msg)
}

func hfError(status int, body []byte) error {
	var parsed struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
		msg = parsed.Error
	}
	if len(msg) > 300 {
		msg = strings.ToValidUTF8(msg[:300], "") + "..."
	}
	return &hfStatusError{Status: status, Msg: msg}
}

// ---------- Jev Decision Index ----------

// indexModel is one model of the Jev Decision Index that has weights on Hugging Face.
type indexModel struct {
	Engine    string  `json:"engine"`
	Name      string  `json:"name"`
	Repo      string  `json:"repo"`
	Params    int64   `json:"params"`
	Kind      string  `json:"kind,omitempty"`
	BaseModel string  `json:"base_model,omitempty"`
	Score     float64 `json:"score"`
	LatencyMS float64 `json:"latency_ms"`
	Coverage  float64 `json:"coverage"`
}

var indexCache struct {
	sync.Mutex
	at        time.Time
	generated string
	models    []indexModel
}

func jevIndex(token string, refresh bool) (string, []indexModel, error) {
	indexCache.Lock()
	defer indexCache.Unlock()
	if !refresh && indexCache.models != nil && time.Since(indexCache.at) < hfIndexTTL {
		return indexCache.generated, indexCache.models, nil
	}
	var raw struct {
		Generated string `json:"generated_utc"`
		Models    []struct {
			Engine   string  `json:"engine"`
			Name     string  `json:"name"`
			Coverage float64 `json:"coverage"`
			Latency  struct {
				Median float64 `json:"median"`
			} `json:"latency"`
			Scores struct {
				BalancedSkill float64 `json:"balanced_skill"`
			} `json:"scores"`
			Meta struct {
				Kind         string `json:"kind"`
				ServedParams int64  `json:"served_params"`
				ModelURL     string `json:"model_url"`
				WeightsRepo  string `json:"weights_repo"`
				BaseModel    string `json:"base_model"`
			} `json:"meta"`
		} `json:"models"`
	}
	if _, errGet := hfJSON(token, hfBase+hfIndexSpace+"/resolve/main/data/index.json", &raw); errGet != nil {
		return "", nil, errGet
	}
	models := make([]indexModel, 0, len(raw.Models))
	seen := map[string]bool{}
	for _, m := range raw.Models {
		repo := repoFromURL(m.Meta.WeightsRepo)
		if repo == "" {
			repo = repoFromURL(m.Meta.ModelURL)
		}
		if repo == "" || seen[repo] {
			continue
		}
		seen[repo] = true
		models = append(models, indexModel{
			Engine: m.Engine, Name: m.Name, Repo: repo, Params: m.Meta.ServedParams, Kind: m.Meta.Kind,
			BaseModel: m.Meta.BaseModel, Score: m.Scores.BalancedSkill, LatencyMS: m.Latency.Median, Coverage: m.Coverage,
		})
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].Score > models[j].Score })
	indexCache.at, indexCache.generated, indexCache.models = time.Now(), raw.Generated, models
	return raw.Generated, models, nil
}

// repoFromURL accepts "owner/name" or a huggingface.co model URL; spaces and datasets are not models.
func repoFromURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if strings.HasPrefix(s, "huggingface.co/") {
		s = strings.TrimPrefix(s, "huggingface.co/")
	} else if strings.Contains(s, "://") || strings.Contains(strings.SplitN(s, "/", 2)[0], ".") {
		return ""
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 || parts[0] == "spaces" || parts[0] == "datasets" {
		return ""
	}
	repo := parts[0] + "/" + parts[1]
	if !validRepo(repo) {
		return ""
	}
	return repo
}

// ---------- repository files ----------

type repoFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// listRepoFiles lists every file of the repo's main branch, following the tree API's pages.
func listRepoFiles(token, repo string) ([]repoFile, error) {
	next := hfBase + "/api/models/" + repo + "/tree/main?recursive=true"
	var files []repoFile
	for next != "" {
		var page []struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
			LFS  *struct {
				Size int64 `json:"size"`
			} `json:"lfs"`
		}
		header, errGet := hfJSON(token, next, &page)
		if errGet != nil {
			return nil, errGet
		}
		for _, f := range page {
			if f.Type != "file" || !filepath.IsLocal(filepath.FromSlash(f.Path)) {
				continue
			}
			size := f.Size
			if f.LFS != nil {
				size = f.LFS.Size
			}
			files = append(files, repoFile{Path: f.Path, Size: size})
		}
		next = nextLink(header.Get("Link"))
	}
	return files, nil
}

func nextLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		if strings.Contains(part, `rel="next"`) {
			if start, end := strings.Index(part, "<"), strings.Index(part, ">"); start >= 0 && end > start {
				return part[start+1 : end]
			}
		}
	}
	return ""
}

// ---------- downloads ----------

type download struct {
	Repo    string `json:"repo"`
	Status  string `json:"status"` // running, done, failed, canceled
	Error   string `json:"error,omitempty"`
	File    string `json:"file,omitempty"`
	Files   int    `json:"files"`
	Total   int64  `json:"total"`
	Done    int64  `json:"done"`
	Path    string `json:"path"`
	Started int64  `json:"started"`
	cancel  context.CancelFunc
}

var downloads = struct {
	sync.Mutex
	m map[string]*download
}{m: map[string]*download{}}

func repoDir(cfg pluginConfig, repo string) string {
	return filepath.Join(cfg.ModelsDir, filepath.FromSlash(repo))
}

func fileSize(path string) int64 {
	if st, errStat := os.Stat(path); errStat == nil && st.Mode().IsRegular() {
		return st.Size()
	}
	return -1
}

// startDownload checks the disk has room for what is still missing, then downloads in the background.
// Partial files are kept as .part, so a canceled or failed download resumes where it stopped.
func startDownload(cfg pluginConfig, repo string, only []string) (download, error) {
	if !validRepo(repo) {
		return download{}, errors.New("invalid repository name")
	}
	downloads.Lock()
	if d := downloads.m[repo]; d != nil && d.Status == "running" {
		downloads.Unlock()
		return download{}, errors.New("this model is already downloading")
	}
	downloads.Unlock()

	files, errList := listRepoFiles(cfg.HFToken, repo)
	if errList != nil {
		return download{}, errList
	}
	if len(only) > 0 {
		want := map[string]bool{}
		for _, p := range only {
			want[p] = true
		}
		kept := files[:0]
		for _, f := range files {
			if want[f.Path] {
				kept = append(kept, f)
			}
		}
		files = kept
	}
	if len(files) == 0 {
		return download{}, errors.New("no files to download")
	}
	dir := repoDir(cfg, repo)
	var total, missing int64
	for _, f := range files {
		total += f.Size
		final := filepath.Join(dir, filepath.FromSlash(f.Path))
		if fileSize(final) == f.Size {
			continue
		}
		missing += f.Size - max(fileSize(final+partSuffix), 0)
	}
	if free, _ := diskSpace(cfg.ModelsDir); free > 0 && uint64(missing) > free {
		return download{}, fmt.Errorf("not enough disk space: %s still to download, %s free in %s", humanBytes(uint64(missing)), humanBytes(free), cfg.ModelsDir)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := &download{Repo: repo, Status: "running", Files: len(files), Total: total, Path: dir, Started: time.Now().UnixMilli(), cancel: cancel}
	downloads.Lock()
	downloads.m[repo] = d
	snapshot := *d
	downloads.Unlock()
	go runDownload(ctx, cfg.HFToken, d, files)
	return snapshot, nil
}

func runDownload(ctx context.Context, token string, d *download, files []repoFile) {
	var errRun error
	for _, f := range files {
		if errRun = downloadFile(ctx, token, d, f); errRun != nil {
			break
		}
	}
	downloads.Lock()
	defer downloads.Unlock()
	d.File = ""
	switch {
	case errRun == nil:
		d.Status = "done"
	case ctx.Err() != nil:
		d.Status = "canceled"
	default:
		d.Status, d.Error = "failed", errRun.Error()
		hostLog("warn", "systemone: download "+d.Repo+" failed: "+errRun.Error())
	}
}

func addDone(d *download, n int64) {
	downloads.Lock()
	d.Done += n
	downloads.Unlock()
}

type progressWriter struct {
	w io.Writer
	d *download
}

func (p progressWriter) Write(b []byte) (int, error) {
	n, errWrite := p.w.Write(b)
	addDone(p.d, int64(n))
	return n, errWrite
}

func downloadFile(ctx context.Context, token string, d *download, f repoFile) error {
	final := filepath.Join(d.Path, filepath.FromSlash(f.Path))
	if fileSize(final) == f.Size {
		addDone(d, f.Size)
		return nil
	}
	downloads.Lock()
	d.File = f.Path
	downloads.Unlock()
	if errMkdir := os.MkdirAll(filepath.Dir(final), 0o755); errMkdir != nil {
		return errMkdir
	}
	part := final + partSuffix
	offset := max(fileSize(part), 0)
	if offset > f.Size {
		offset = 0
	}
	segments := strings.Split(f.Path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	header := http.Header{}
	if offset > 0 {
		header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, errGet := hfGet(ctx, token, hfBase+"/"+d.Repo+"/resolve/main/"+strings.Join(segments, "/"), header)
	if errGet != nil {
		return errGet
	}
	defer func() { _ = resp.Body.Close() }()
	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset == f.Size:
		addDone(d, offset)
		return os.Rename(part, final)
	case resp.StatusCode == http.StatusOK:
		flags |= os.O_TRUNC
		offset = 0
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %w", f.Path, hfError(resp.StatusCode, body))
	}
	out, errOpen := os.OpenFile(part, flags, 0o644)
	if errOpen != nil {
		return errOpen
	}
	addDone(d, offset)
	_, errCopy := io.Copy(progressWriter{w: out, d: d}, resp.Body)
	if errClose := out.Close(); errCopy == nil {
		errCopy = errClose
	}
	if errCopy != nil {
		return fmt.Errorf("%s: %w", f.Path, errCopy)
	}
	if got := fileSize(part); got != f.Size {
		return fmt.Errorf("%s: got %d bytes, expected %d", f.Path, got, f.Size)
	}
	return os.Rename(part, final)
}

func cancelDownload(repo string) bool {
	downloads.Lock()
	defer downloads.Unlock()
	d := downloads.m[repo]
	if d == nil || d.Status != "running" {
		return false
	}
	d.cancel()
	return true
}

// cancelAllDownloads runs on plugin shutdown; partial files stay for the next start.
func cancelAllDownloads() {
	downloads.Lock()
	defer downloads.Unlock()
	for _, d := range downloads.m {
		if d.cancel != nil {
			d.cancel()
		}
	}
}

func deleteModel(cfg pluginConfig, repo string) error {
	if !validRepo(repo) {
		return errors.New("invalid repository name")
	}
	downloads.Lock()
	d := downloads.m[repo]
	if d != nil && d.Status == "running" {
		downloads.Unlock()
		return errors.New("cancel the download first")
	}
	delete(downloads.m, repo)
	downloads.Unlock()
	if errRemove := os.RemoveAll(repoDir(cfg, repo)); errRemove != nil {
		return errRemove
	}
	_ = os.Remove(filepath.Dir(repoDir(cfg, repo))) // drop the owner folder once empty
	return nil
}

// localModel is a model folder found under models-dir.
type localModel struct {
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Files   int    `json:"files"`
	Partial bool   `json:"partial"`
}

func localModels(cfg pluginConfig) []localModel {
	owners, _ := os.ReadDir(cfg.ModelsDir)
	out := []localModel{}
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		names, _ := os.ReadDir(filepath.Join(cfg.ModelsDir, owner.Name()))
		for _, name := range names {
			if !name.IsDir() {
				continue
			}
			m := localModel{Repo: owner.Name() + "/" + name.Name()}
			m.Path = repoDir(cfg, m.Repo)
			_ = filepath.WalkDir(m.Path, func(_ string, e fs.DirEntry, errWalk error) error {
				if errWalk != nil || e.IsDir() {
					return nil
				}
				if info, errInfo := e.Info(); errInfo == nil {
					m.Size += info.Size()
				}
				if strings.HasSuffix(e.Name(), partSuffix) {
					m.Partial = true
				} else {
					m.Files++
				}
				return nil
			})
			out = append(out, m)
		}
	}
	return out
}

func downloadList() []download {
	downloads.Lock()
	defer downloads.Unlock()
	out := make([]download, 0, len(downloads.m))
	for _, d := range downloads.m {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	return out
}

// ---------- token ----------

var whoamiCache sync.Map // token -> user name

func hfUser(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	if name, ok := whoamiCache.Load(token); ok {
		return name.(string), nil
	}
	var who struct {
		Name string `json:"name"`
	}
	if _, errGet := hfJSON(token, hfBase+"/api/whoami-v2", &who); errGet != nil {
		var statusErr *hfStatusError
		if errors.As(errGet, &statusErr) && statusErr.Status == http.StatusUnauthorized {
			return "", errors.New("Hugging Face did not accept this token. Check that it was copied in full and has not been revoked.")
		}
		return "", errGet
	}
	whoamiCache.Store(token, who.Name)
	return who.Name, nil
}

// ---------- machine ----------

type gpuInfo struct {
	Name        string `json:"name"`
	MemoryTotal uint64 `json:"memory_total"`
	MemoryFree  uint64 `json:"memory_free"`
}

type machineInfo struct {
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	CPU          string    `json:"cpu,omitempty"`
	Cores        int       `json:"cores"`
	RAMTotal     uint64    `json:"ram_total"`
	RAMAvailable uint64    `json:"ram_available"`
	DiskFree     uint64    `json:"disk_free"`
	DiskTotal    uint64    `json:"disk_total"`
	DiskPath     string    `json:"disk_path"`
	GPUs         []gpuInfo `json:"gpus"`
}

// machine describes the computer CLIProxyAPI runs on, which is where models are downloaded.
func machine(cfg pluginConfig) machineInfo {
	m := machineInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, Cores: runtime.NumCPU(), DiskPath: cfg.ModelsDir, GPUs: nvidiaGPUs()}
	if abs, errAbs := filepath.Abs(cfg.ModelsDir); errAbs == nil {
		m.DiskPath = abs
	}
	m.DiskFree, m.DiskTotal = diskSpace(cfg.ModelsDir)
	m.CPU = procField("/proc/cpuinfo", "model name")
	mem := map[string]uint64{}
	for _, k := range []string{"MemTotal", "MemAvailable"} {
		if kb, errParse := strconv.ParseUint(strings.TrimSuffix(procField("/proc/meminfo", k), " kB"), 10, 64); errParse == nil {
			mem[k] = kb * 1024
		}
	}
	m.RAMTotal, m.RAMAvailable = mem["MemTotal"], mem["MemAvailable"]
	// Inside a container /proc/meminfo shows the host; a cgroup memory limit is what the models really get.
	if limit, errLimit := readUint("/sys/fs/cgroup/memory.max"); errLimit == nil && limit > 0 && (m.RAMTotal == 0 || limit < m.RAMTotal) {
		used, _ := readUint("/sys/fs/cgroup/memory.current")
		m.RAMTotal = limit
		if used < limit && (m.RAMAvailable == 0 || limit-used < m.RAMAvailable) {
			m.RAMAvailable = limit - used
		}
	}
	return m
}

func procField(path, key string) string {
	f, errOpen := os.Open(path)
	if errOpen != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if k, v, ok := strings.Cut(scanner.Text(), ":"); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func readUint(path string) (uint64, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return 0, errRead
	}
	return strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
}

// nvidiaGPUs asks nvidia-smi when it is installed; other GPUs are not detected.
func nvidiaGPUs() []gpuInfo {
	bin, errLook := exec.LookPath("nvidia-smi")
	if errLook != nil {
		return []gpuInfo{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, errRun := exec.CommandContext(ctx, bin, "--query-gpu=name,memory.total,memory.free", "--format=csv,noheader,nounits").Output()
	if errRun != nil {
		return []gpuInfo{}
	}
	gpus := []gpuInfo{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		cols := strings.Split(line, ",")
		if len(cols) != 3 {
			continue
		}
		total, _ := strconv.ParseUint(strings.TrimSpace(cols[1]), 10, 64)
		free, _ := strconv.ParseUint(strings.TrimSpace(cols[2]), 10, 64)
		gpus = append(gpus, gpuInfo{Name: strings.TrimSpace(cols[0]), MemoryTotal: total << 20, MemoryFree: free << 20})
	}
	return gpus
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
