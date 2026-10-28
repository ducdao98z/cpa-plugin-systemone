package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Models run in llama.cpp server containers that the plugin creates through the Docker Engine API.
// llama.cpp answers /v1/systemone itself once the GGUF file carries decision metadata (see gguf.go).
const (
	llamaImageGPU = "ghcr.io/ggml-org/llama.cpp:server-cuda"
	llamaImageCPU = "ghcr.io/ggml-org/llama.cpp:server"
	runnerLabel   = "systemone.model"
	runnerPrefix  = "systemone-"
	runnerPort    = "8080"
)

// ---------- Docker Engine API ----------

func dockerSocket() string {
	if host := os.Getenv("DOCKER_HOST"); strings.HasPrefix(host, "unix://") {
		return strings.TrimPrefix(host, "unix://")
	}
	return "/var/run/docker.sock"
}

var dockerHTTP = &http.Client{Transport: &http.Transport{
	DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", dockerSocket())
	},
}}

// dockerCall sends a JSON request to the Docker Engine and decodes a JSON answer into out when given.
func dockerCall(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return errMarshal
		}
		reader = bytes.NewReader(raw)
	}
	req, errNew := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if errNew != nil {
		return errNew
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, errDo := dockerHTTP.Do(req)
	if errDo != nil {
		return fmt.Errorf("Docker is not reachable at %s: %w", dockerSocket(), errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		var msg struct {
			Message string `json:"message"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(raw, &msg) != nil || msg.Message == "" {
			msg.Message = strings.TrimSpace(string(raw))
		}
		return &dockerError{Status: resp.StatusCode, Msg: msg.Message}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type dockerError struct {
	Status int
	Msg    string
}

func (e *dockerError) Error() string { return fmt.Sprintf("Docker answered %d: %s", e.Status, e.Msg) }

func isDockerStatus(err error, status int) bool {
	var de *dockerError
	return errors.As(err, &de) && de.Status == status
}

type mountPoint struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// dockerEnv is where new model containers go: the network and mounts of CLIProxyAPI's own container,
// or nothing when CLIProxyAPI runs directly on the host.
type dockerEnv struct {
	InContainer bool         `json:"in_container"`
	Network     string       `json:"network,omitempty"`
	Mounts      []mountPoint `json:"-"`
}

func currentDockerEnv(ctx context.Context) (dockerEnv, error) {
	if errPing := dockerCall(ctx, http.MethodGet, "/_ping", nil, nil); errPing != nil {
		return dockerEnv{}, errPing
	}
	if _, errStat := os.Stat("/.dockerenv"); errStat != nil {
		return dockerEnv{}, nil
	}
	self, _ := os.Hostname()
	var info struct {
		Mounts          []mountPoint `json:"Mounts"`
		NetworkSettings struct {
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if errInspect := dockerCall(ctx, http.MethodGet, "/containers/"+url.PathEscape(self)+"/json", nil, &info); errInspect != nil {
		return dockerEnv{}, fmt.Errorf("cannot find CLIProxyAPI's own container (%s): %w", self, errInspect)
	}
	env := dockerEnv{InContainer: true, Mounts: info.Mounts}
	names := make([]string, 0, len(info.NetworkSettings.Networks))
	for name := range info.NetworkSettings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 0 {
		env.Network = names[0]
	}
	return env, nil
}

// hostPath turns a path inside CLIProxyAPI's container into the host path Docker can mount,
// through the longest bind mount that holds it.
func hostPath(env dockerEnv, p string) (string, error) {
	abs, errAbs := filepath.Abs(p)
	if errAbs != nil {
		return "", errAbs
	}
	if !env.InContainer {
		return abs, nil
	}
	best := mountPoint{}
	for _, m := range env.Mounts {
		if (abs == m.Destination || strings.HasPrefix(abs, strings.TrimSuffix(m.Destination, "/")+"/")) && len(m.Destination) > len(best.Destination) {
			best = m
		}
	}
	if best.Destination == "" {
		return "", fmt.Errorf("%s is not on a folder mounted from the host, so a model container cannot see it. Choose a model folder under one of CLIProxyAPI's mounts", abs)
	}
	return best.Source + strings.TrimPrefix(abs, best.Destination), nil
}

// ---------- decision settings of the original model ----------

type decisionSettings struct {
	Source    string `json:"source,omitempty"` // repo the settings came from
	Readout   string `json:"readout"`
	Noncausal bool   `json:"noncausal"`
}

// readDecisionSettings finds how a decision model was trained: config.json modeling_overrides, or
// readout.json for checkpoints without overrides. A GGUF repo usually lacks both, so its base models
// on Hugging Face are asked as well.
func readDecisionSettings(cfg pluginConfig, repo string) (decisionSettings, error) {
	candidates := []string{repo}
	var info struct {
		CardData struct {
			BaseModel any `json:"base_model"`
		} `json:"cardData"`
	}
	if _, errInfo := hfJSON(cfg.HFToken, hfBase+"/api/models/"+repo, &info); errInfo == nil {
		switch v := info.CardData.BaseModel.(type) {
		case string:
			candidates = append(candidates, v)
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					candidates = append(candidates, s)
				}
			}
		}
	}
	for _, c := range candidates {
		if !validRepo(c) {
			continue
		}
		var model struct {
			Overrides *struct {
				Attention map[string]string `json:"attention"`
				Readout   string            `json:"readout"`
			} `json:"modeling_overrides"`
		}
		var readout struct {
			Attention string `json:"attention"`
			Readout   string `json:"readout"`
		}
		hasModel := repoJSON(cfg, c, "config.json", &model) == nil && model.Overrides != nil
		hasReadout := repoJSON(cfg, c, "readout.json", &readout) == nil && readout.Readout != ""
		if !hasModel && !hasReadout {
			continue
		}
		s := decisionSettings{Source: c, Readout: readout.Readout, Noncausal: readout.Attention == "noncausal"}
		if hasModel {
			if model.Overrides.Readout != "" {
				s.Readout = model.Overrides.Readout
			}
			s.Noncausal = model.Overrides.Attention["full_attention"] == "block_bidirectional"
		}
		if s.Readout != "slot_lm" {
			return s, fmt.Errorf("%s uses the %q readout; llama.cpp only runs slot_lm decision models", c, s.Readout)
		}
		return s, nil
	}
	// Not a declared decision checkpoint: read the answer letter's logits with the model's own attention.
	return decisionSettings{Readout: "slot_lm"}, nil
}

// repoJSON reads a small file of a repo, from the download folder when present, else from Hugging Face.
func repoJSON(cfg pluginConfig, repo, file string, out any) error {
	if raw, errRead := os.ReadFile(filepath.Join(repoDir(cfg, repo), file)); errRead == nil {
		return json.Unmarshal(raw, out)
	}
	_, errGet := hfJSON(cfg.HFToken, hfBase+"/"+repo+"/resolve/main/"+file, out)
	return errGet
}

// ggufBuilds lists the GGUF conversions Hugging Face knows for a model, most downloaded first.
func ggufBuilds(token, repo string) ([]string, error) {
	var found []struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	q := url.Values{"filter": {"base_model:quantized:" + repo}, "sort": {"downloads"}, "limit": {"30"}}
	if _, errGet := hfJSON(token, hfBase+"/api/models?"+q.Encode(), &found); errGet != nil {
		return nil, errGet
	}
	out := []string{}
	for _, m := range found {
		for _, t := range m.Tags {
			if t == "gguf" {
				out = append(out, m.ID)
				break
			}
		}
	}
	return out, nil
}

// ---------- local GGUF files ----------

type ggufFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Arch   string `json:"arch,omitempty"`
	MMProj bool   `json:"mmproj"`
	Ready  bool   `json:"ready"` // has decision metadata already
	Error  string `json:"error,omitempty"`
}

func localGGUF(cfg pluginConfig, repo string) []ggufFile {
	dir := repoDir(cfg, repo)
	out := []ggufFile{}
	_ = filepath.WalkDir(dir, func(p string, e os.DirEntry, errWalk error) error {
		if errWalk != nil || e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		f := ggufFile{Path: filepath.ToSlash(rel), Size: fileSize(p), MMProj: strings.Contains(strings.ToLower(e.Name()), "mmproj")}
		if m, errRead := readGGUF(p); errRead != nil {
			f.Error = errRead.Error()
		} else {
			f.Arch, f.Ready = m.Arch, m.decisionReady()
			f.MMProj = f.MMProj || m.Arch == "clip"
		}
		out = append(out, f)
		return nil
	})
	return out
}

// ---------- runners ----------

type runRequest struct {
	Repo    string `json:"repo"`
	File    string `json:"file"`
	MMProj  string `json:"mmproj"`
	Name    string `json:"name"`
	Context int    `json:"context"`
	Port    int    `json:"port"`
}

var runnerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// runJob is the install-and-start progress of one model, shown on the page while it runs.
type runJob struct {
	Name    string `json:"name"`
	Repo    string `json:"repo"`
	Step    string `json:"step"` // preparing, pulling, starting, loading, ready, failed
	Error   string `json:"error,omitempty"`
	Done    int64  `json:"done"`
	Total   int64  `json:"total"`
	URL     string `json:"url"`
	Started int64  `json:"started"`
}

var runJobs = struct {
	sync.Mutex
	m      map[string]*runJob
	ctx    context.Context
	cancel context.CancelFunc
}{m: map[string]*runJob{}}

func jobContext() context.Context {
	runJobs.Lock()
	defer runJobs.Unlock()
	if runJobs.ctx == nil {
		runJobs.ctx, runJobs.cancel = context.WithCancel(context.Background())
	}
	return runJobs.ctx
}

// cancelRunJobs runs on plugin shutdown. Containers keep running; they restart with Docker.
func cancelRunJobs() {
	runJobs.Lock()
	defer runJobs.Unlock()
	if runJobs.cancel != nil {
		runJobs.cancel()
		runJobs.ctx, runJobs.cancel = nil, nil
	}
}

func setJob(j *runJob, update func(*runJob)) {
	runJobs.Lock()
	update(j)
	runJobs.Unlock()
}

func jobList() []runJob {
	runJobs.Lock()
	defer runJobs.Unlock()
	out := make([]runJob, 0, len(runJobs.m))
	for _, j := range runJobs.m {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Started > out[k].Started })
	return out
}

// runnerURL is the System One URL of a model container, as CLIProxyAPI reaches it.
func runnerURL(env dockerEnv, container string, port int) string {
	if env.InContainer && env.Network != "" {
		return "http://" + container + ":" + runnerPort + "/v1/systemone"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1/systemone", port)
}

// startRunner validates the request and runs the install steps in the background.
func startRunner(cfg pluginConfig, req runRequest) (runJob, error) {
	if !validRepo(req.Repo) {
		return runJob{}, errors.New("invalid repository name")
	}
	if !runnerName.MatchString(req.Name) {
		return runJob{}, errors.New("the model name may use letters, digits, '.', '_' and '-' (up to 63)")
	}
	dir := repoDir(cfg, req.Repo)
	for _, f := range []string{req.File, req.MMProj} {
		if f != "" && (!filepath.IsLocal(filepath.FromSlash(f)) || fileSize(filepath.Join(dir, filepath.FromSlash(f))) < 0) {
			return runJob{}, fmt.Errorf("%s is not a downloaded file of %s", f, req.Repo)
		}
	}
	if req.File == "" {
		return runJob{}, errors.New("choose a GGUF model file")
	}
	if req.Context <= 0 {
		req.Context = 32768
	}
	ctx := jobContext()
	env, errEnv := currentDockerEnv(ctx)
	if errEnv != nil {
		return runJob{}, errEnv
	}
	if !(env.InContainer && env.Network != "") && (req.Port <= 0 || req.Port > 65535) {
		return runJob{}, errors.New("enter a host port for the model server")
	}
	hostDir, errHost := hostPath(env, dir)
	if errHost != nil {
		return runJob{}, errHost
	}
	container := runnerPrefix + req.Name
	runJobs.Lock()
	if j := runJobs.m[req.Name]; j != nil && j.Step != "ready" && j.Step != "failed" {
		runJobs.Unlock()
		return runJob{}, errors.New("this model is already being set up")
	}
	j := &runJob{Name: req.Name, Repo: req.Repo, Step: "preparing", URL: runnerURL(env, container, req.Port), Started: time.Now().UnixMilli()}
	runJobs.m[req.Name] = j
	snapshot := *j
	runJobs.Unlock()
	go func() {
		errRun := runSteps(ctx, cfg, env, hostDir, container, req, j)
		setJob(j, func(j *runJob) {
			if errRun != nil {
				j.Step, j.Error = "failed", errRun.Error()
			} else {
				j.Step = "ready"
			}
		})
		if errRun != nil {
			hostLog("warn", "systemone: starting "+req.Name+" failed: "+errRun.Error())
		}
	}()
	return snapshot, nil
}

func runSteps(ctx context.Context, cfg pluginConfig, env dockerEnv, hostDir, container string, req runRequest, j *runJob) error {
	dir := repoDir(cfg, req.Repo)
	model := req.File
	meta, errRead := readGGUF(filepath.Join(dir, filepath.FromSlash(model)))
	if errRead != nil {
		return fmt.Errorf("%s: %w", model, errRead)
	}
	if !meta.decisionReady() {
		settings, errSettings := readDecisionSettings(cfg, req.Repo)
		if errSettings != nil {
			return errSettings
		}
		src := filepath.Join(dir, filepath.FromSlash(model))
		model = systemoneName(model)
		dst := filepath.Join(dir, filepath.FromSlash(model))
		if ready, errReady := readGGUF(dst); errReady != nil || !ready.decisionReady() {
			size := fileSize(src)
			if free, _ := diskSpace(dir); free > 0 && uint64(size) > free {
				return fmt.Errorf("not enough disk space to add decision metadata: %s needed, %s free", humanBytes(uint64(size)), humanBytes(free))
			}
			setJob(j, func(j *runJob) { j.Total = size - meta.dataStart })
			if errGraft := graftDecision(src, dst, settings.Noncausal, func(n int64) { setJob(j, func(j *runJob) { j.Done += n }) }); errGraft != nil {
				return errGraft
			}
		}
	}

	image := llamaImageCPU
	gpu := len(nvidiaGPUs()) > 0
	if gpu {
		image = llamaImageGPU
	}
	if errImage := dockerCall(ctx, http.MethodGet, "/images/"+image+"/json", nil, nil); isDockerStatus(errImage, http.StatusNotFound) {
		setJob(j, func(j *runJob) { j.Step, j.Done, j.Total = "pulling", 0, 0 })
		if errPull := pullImage(ctx, image); errPull != nil {
			return errPull
		}
	} else if errImage != nil {
		return errImage
	}

	setJob(j, func(j *runJob) { j.Step = "starting" })
	if errRemove := removeRunner(ctx, container); errRemove != nil && !isDockerStatus(errRemove, http.StatusNotFound) {
		return errRemove
	}
	args := []string{"-m", "/models/" + model}
	if req.MMProj != "" {
		args = append(args, "--mmproj", "/models/"+req.MMProj, "--mmproj-offload", "--image-min-tokens", "1024")
	}
	args = append(args, "--alias", req.Name, "-ngl", "99", "-np", "1", "-c", fmt.Sprint(req.Context), "-fa", "on", "--jinja",
		"--cache-ram", "0", "-b", "2048", "-ub", "2048", "--host", "0.0.0.0", "--port", runnerPort)
	host := map[string]any{
		"Binds":         []string{hostDir + ":/models:ro"},
		"RestartPolicy": map[string]string{"Name": "unless-stopped"},
	}
	if gpu {
		host["DeviceRequests"] = []map[string]any{{"Count": -1, "Capabilities": [][]string{{"gpu"}}}}
	}
	body := map[string]any{
		"Image": image, "Cmd": args, "HostConfig": host,
		"Labels": map[string]string{runnerLabel: req.Name, "systemone.repo": req.Repo, "systemone.file": model, "systemone.url": j.URL},
	}
	if req.Port > 0 {
		body["ExposedPorts"] = map[string]any{runnerPort + "/tcp": map[string]any{}}
		host["PortBindings"] = map[string]any{runnerPort + "/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": fmt.Sprint(req.Port)}}}
	}
	if env.InContainer && env.Network != "" {
		body["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{env.Network: map[string]any{}}}
	}
	if errCreate := dockerCall(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(container), body, nil); errCreate != nil {
		return errCreate
	}
	if errStart := dockerCall(ctx, http.MethodPost, "/containers/"+container+"/start", nil, nil); errStart != nil {
		return errStart
	}

	setJob(j, func(j *runJob) { j.Step = "loading" })
	health := strings.TrimSuffix(j.URL, "/v1/systemone") + "/health"
	for {
		if probe, errNew := http.NewRequestWithContext(ctx, http.MethodGet, health, nil); errNew == nil {
			if resp, errGet := http.DefaultClient.Do(probe); errGet == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		var state struct {
			State struct {
				Running  bool `json:"Running"`
				ExitCode int  `json:"ExitCode"`
			} `json:"State"`
		}
		if errInspect := dockerCall(ctx, http.MethodGet, "/containers/"+container+"/json", nil, &state); errInspect != nil {
			return errInspect
		}
		if !state.State.Running {
			logs, _ := runnerLogs(ctx, container, 20)
			return fmt.Errorf("the model server stopped (exit %d): %s", state.State.ExitCode, logs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// pullImage downloads a container image; Docker streams JSON progress lines and reports errors in them.
func pullImage(ctx context.Context, image string) error {
	name, tag, _ := strings.Cut(image[strings.LastIndex(image, "/")+1:], ":")
	repo := image[:strings.LastIndex(image, "/")+1] + name
	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/images/create?"+url.Values{"fromImage": {repo}, "tag": {tag}}.Encode(), nil)
	if errNew != nil {
		return errNew
	}
	resp, errDo := dockerHTTP.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = resp.Body.Close() }()
	dec := json.NewDecoder(resp.Body)
	for {
		var line struct {
			Error string `json:"error"`
		}
		if errDecode := dec.Decode(&line); errDecode == io.EOF {
			return nil
		} else if errDecode != nil {
			return errDecode
		}
		if line.Error != "" {
			return errors.New("pulling " + image + ": " + line.Error)
		}
	}
}

type runner struct {
	Container string `json:"container"`
	Name      string `json:"name"`
	Repo      string `json:"repo"`
	File      string `json:"file"`
	URL       string `json:"url"`
	State     string `json:"state"`
	Status    string `json:"status"`
}

func listRunners(ctx context.Context) ([]runner, error) {
	var found []struct {
		Names  []string          `json:"Names"`
		State  string            `json:"State"`
		Status string            `json:"Status"`
		Labels map[string]string `json:"Labels"`
	}
	filters, _ := json.Marshal(map[string][]string{"label": {runnerLabel}})
	if errList := dockerCall(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil, &found); errList != nil {
		return nil, errList
	}
	out := []runner{}
	for _, c := range found {
		r := runner{Name: c.Labels[runnerLabel], Repo: c.Labels["systemone.repo"], File: c.Labels["systemone.file"], URL: c.Labels["systemone.url"], State: c.State, Status: c.Status}
		if len(c.Names) > 0 {
			r.Container = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out, nil
}

// removeRunner deletes a container the plugin created; other containers are left alone.
func removeRunner(ctx context.Context, container string) error {
	var info struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if errInspect := dockerCall(ctx, http.MethodGet, "/containers/"+url.PathEscape(container)+"/json", nil, &info); errInspect != nil {
		return errInspect
	}
	if info.Config.Labels[runnerLabel] == "" {
		return fmt.Errorf("%s was not created by Open Systemone", container)
	}
	return dockerCall(ctx, http.MethodDelete, "/containers/"+url.PathEscape(container)+"?force=1", nil, nil)
}

// runnerLogs returns the last lines a model container printed.
func runnerLogs(ctx context.Context, container string, tail int) (string, error) {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://docker/containers/%s/logs?stdout=1&stderr=1&tail=%d", url.PathEscape(container), tail), nil)
	if errNew != nil {
		return "", errNew
	}
	resp, errDo := dockerHTTP.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer func() { _ = resp.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return "", errRead
	}
	if resp.StatusCode != http.StatusOK {
		return "", &dockerError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(raw))}
	}
	return demuxLogs(raw), nil
}

// demuxLogs strips the 8-byte stream headers Docker puts on the output of containers without a TTY.
func demuxLogs(raw []byte) string {
	var out strings.Builder
	for len(raw) >= 8 && raw[0] <= 2 && raw[1] == 0 && raw[2] == 0 && raw[3] == 0 {
		n := int(binary.BigEndian.Uint32(raw[4:8]))
		if 8+n > len(raw) {
			break
		}
		out.Write(raw[8 : 8+n])
		raw = raw[8+n:]
	}
	out.Write(raw)
	return out.String()
}

// runnerAction stops, starts or deletes one of the plugin's model containers.
func runnerAction(ctx context.Context, name, action string) error {
	if !runnerName.MatchString(name) {
		return errors.New("invalid model name")
	}
	container := runnerPrefix + name
	switch action {
	case "stop", "start", "restart":
		return dockerCall(ctx, http.MethodPost, "/containers/"+container+"/"+action, nil, nil)
	case "remove":
		errRemove := removeRunner(ctx, container)
		runJobs.Lock()
		delete(runJobs.m, name)
		runJobs.Unlock()
		return errRemove
	}
	return errors.New("unknown action")
}
