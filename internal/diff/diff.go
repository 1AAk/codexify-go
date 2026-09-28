package diff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/benice2me11/codexify-go/internal/config"
)

const (
	snapshotMessage = "Codexify Go diff snapshot"
	refRoot         = "refs/codexify-go/diff"
)

var ErrNotGitWorktree = errors.New("show_diff requires a Git worktree")

type Baseline string

const (
	BaselineLastDiff    Baseline = "last_diff"
	BaselineProjectOpen Baseline = "project_open"
)

func ParseBaseline(raw string) (Baseline, error) {
	switch strings.TrimSpace(raw) {
	case "", string(BaselineLastDiff):
		return BaselineLastDiff, nil
	case string(BaselineProjectOpen):
		return BaselineProjectOpen, nil
	default:
		return "", fmt.Errorf("since must be %q or %q", BaselineLastDiff, BaselineProjectOpen)
	}
}

type Owner struct {
	Key        string
	Persistent bool
}

type Request struct {
	Since        Baseline
	Advance      bool
	IncludePatch bool
}

type Summary struct {
	Files       int    `json:"files"`
	Additions   uint64 `json:"additions"`
	Deletions   uint64 `json:"deletions"`
	BinaryFiles int    `json:"binaryFiles"`
}

type File struct {
	Path         string  `json:"path"`
	PreviousPath string  `json:"previousPath,omitempty"`
	Status       string  `json:"status"`
	Additions    *uint64 `json:"additions,omitempty"`
	Deletions    *uint64 `json:"deletions,omitempty"`
	Binary       bool    `json:"binary"`
}

type Result struct {
	Since              Baseline `json:"since"`
	AdvanceRequested   bool     `json:"advanceRequested"`
	CheckpointAdvanced bool     `json:"checkpointAdvanced"`
	Scope              string   `json:"scope"`
	Summary            Summary  `json:"summary"`
	Files              []File   `json:"files"`
	FilesOmitted       int      `json:"filesOmitted"`
	Patch              string   `json:"patch"`
	PatchIncluded      bool     `json:"patchIncluded"`
	PatchBytes         *int     `json:"patchBytes,omitempty"`
	PatchOmittedReason string   `json:"patchOmittedReason,omitempty"`
	Warnings           []string `json:"warnings"`
}

func (r Result) RenderText() string {
	var lines []string
	label := "last diff"
	if r.Since == BaselineProjectOpen {
		label = "project open"
	}
	if r.Summary.Files == 0 {
		lines = append(lines, fmt.Sprintf("No changes since %s.", label))
	} else {
		suffix := "s"
		if r.Summary.Files == 1 {
			suffix = ""
		}
		lines = append(lines, fmt.Sprintf(
			"Changes since %s: %d file%s (+%d -%d, %d binary).",
			label, r.Summary.Files, suffix, r.Summary.Additions, r.Summary.Deletions, r.Summary.BinaryFiles,
		))
	}
	lines = append(lines, "Repository scope: "+r.Scope)
	if r.FilesOmitted > 0 {
		lines = append(lines, fmt.Sprintf("%d additional file record(s) omitted.", r.FilesOmitted))
	}
	if r.AdvanceRequested {
		if r.CheckpointAdvanced {
			lines = append(lines, "Last-diff checkpoint advanced.")
		} else {
			lines = append(lines, "Last-diff checkpoint was not advanced.")
		}
	}
	for _, warning := range r.Warnings {
		lines = append(lines, "Warning: "+warning)
	}
	if r.PatchIncluded && r.Patch != "" {
		n := len(r.Patch)
		if r.PatchBytes != nil {
			n = *r.PatchBytes
		}
		lines = append(lines, fmt.Sprintf("Complete patch attached to component metadata (%d bytes).", n))
	} else if r.PatchOmittedReason != "" {
		lines = append(lines, "Patch omitted: "+r.PatchOmittedReason)
	}
	return strings.Join(lines, "\n")
}

type Manager struct {
	cfg config.DiffConfig

	mu        sync.Mutex
	transient map[string]*transientCheckpoint
}

type transientCheckpoint struct {
	pair      checkpointPair
	tempRoot  string
	objects   string
	alternate string
}

type checkpointPair struct {
	projectOpen string
	lastDiff    string
}

type workspace struct {
	gitRoot  string
	pathspec string
	scope    string
	key      string
}

func New(cfg config.DiffConfig) *Manager {
	return &Manager{
		cfg:       cfg,
		transient: make(map[string]*transientCheckpoint),
	}
}

func (m *Manager) Forget(owner Owner) {
	if owner.Persistent || strings.TrimSpace(owner.Key) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := ownerKey(owner) + ":"
	for key, state := range m.transient {
		if strings.HasPrefix(key, prefix) {
			if state.tempRoot != "" {
				_ = os.RemoveAll(state.tempRoot)
			}
			delete(m.transient, key)
		}
	}
}

func (m *Manager) Ensure(workDir string, owner Owner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, err := resolveWorkspace(workDir)
	if err != nil {
		return err
	}
	_, _, err = m.loadOrInit(ws, owner)
	return err
}

func (m *Manager) Show(workDir string, owner Owner, req Request) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ws, err := resolveWorkspace(workDir)
	if err != nil {
		return Result{}, err
	}
	pair, objectEnv, err := m.loadOrInit(ws, owner)
	if err != nil {
		return Result{}, err
	}
	current, err := createSnapshot(ws, objectEnv)
	if err != nil {
		return Result{}, err
	}

	baseline := pair.lastDiff
	if req.Since == BaselineProjectOpen {
		baseline = pair.projectOpen
	}
	result, err := m.compare(ws, baseline, current, req, objectEnv)
	if err != nil {
		return Result{}, err
	}
	if req.Advance {
		if owner.Persistent {
			advanced, err := compareAndSwapRef(ws.gitRoot, lastDiffRef(ws, owner), current, pair.lastDiff)
			if err != nil {
				return Result{}, err
			}
			result.CheckpointAdvanced = advanced
			if !advanced {
				result.Warnings = append(result.Warnings, "the last-diff checkpoint changed concurrently; call show_diff again")
			}
		} else {
			key := transientKey(ws, owner)
			state := m.transient[key]
			if state == nil {
				return Result{}, errors.New("transport diff checkpoint disappeared")
			}
			state.pair.lastDiff = current
			result.CheckpointAdvanced = true
		}
	}
	return result, nil
}

func (m *Manager) loadOrInit(ws workspace, owner Owner) (checkpointPair, map[string]string, error) {
	if strings.TrimSpace(owner.Key) == "" {
		return checkpointPair{}, nil, errors.New("diff owner key is required")
	}
	if owner.Persistent {
		projectOpen, err := readRef(ws.gitRoot, projectOpenRef(ws, owner))
		if err != nil {
			return checkpointPair{}, nil, err
		}
		lastDiff, err := readRef(ws.gitRoot, lastDiffRef(ws, owner))
		if err != nil {
			return checkpointPair{}, nil, err
		}
		switch {
		case projectOpen != "" && lastDiff != "":
			return checkpointPair{projectOpen: projectOpen, lastDiff: lastDiff}, nil, nil
		case projectOpen != "" && lastDiff == "":
			if _, err := createRefIfAbsent(ws.gitRoot, lastDiffRef(ws, owner), projectOpen); err != nil {
				return checkpointPair{}, nil, err
			}
			lastDiff, err = readRef(ws.gitRoot, lastDiffRef(ws, owner))
			if err != nil {
				return checkpointPair{}, nil, err
			}
			return checkpointPair{projectOpen: projectOpen, lastDiff: lastDiff}, nil, nil
		case projectOpen == "" && lastDiff != "":
			return checkpointPair{}, nil, errors.New("diff checkpoint state is inconsistent: last-diff exists without project-open")
		default:
			snapshot, err := createSnapshot(ws, nil)
			if err != nil {
				return checkpointPair{}, nil, err
			}
			if _, err := createRefIfAbsent(ws.gitRoot, projectOpenRef(ws, owner), snapshot); err != nil {
				return checkpointPair{}, nil, err
			}
			projectOpen, err = readRef(ws.gitRoot, projectOpenRef(ws, owner))
			if err != nil {
				return checkpointPair{}, nil, err
			}
			if _, err := createRefIfAbsent(ws.gitRoot, lastDiffRef(ws, owner), projectOpen); err != nil {
				return checkpointPair{}, nil, err
			}
			lastDiff, err = readRef(ws.gitRoot, lastDiffRef(ws, owner))
			if err != nil {
				return checkpointPair{}, nil, err
			}
			return checkpointPair{projectOpen: projectOpen, lastDiff: lastDiff}, nil, nil
		}
	}

	key := transientKey(ws, owner)
	if state := m.transient[key]; state != nil {
		return state.pair, state.objectEnv(), nil
	}
	state, err := createTransientCheckpoint(ws)
	if err != nil {
		return checkpointPair{}, nil, err
	}
	m.transient[key] = state
	return state.pair, state.objectEnv(), nil
}

func createTransientCheckpoint(ws workspace) (*transientCheckpoint, error) {
	objectsPath, err := gitChecked(ws.gitRoot, []string{"rev-parse", "--git-path", "objects"}, nil, nil)
	if err != nil {
		return nil, err
	}
	alternate := strings.TrimSpace(string(objectsPath))
	if !filepath.IsAbs(alternate) {
		alternate = filepath.Join(ws.gitRoot, alternate)
	}
	alternate, err = filepath.Abs(alternate)
	if err != nil {
		return nil, err
	}

	tempRoot, err := os.MkdirTemp("", "codexify-go-diff-*")
	if err != nil {
		return nil, err
	}
	objects := filepath.Join(tempRoot, "objects")
	if err := os.Mkdir(objects, 0o700); err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	state := &transientCheckpoint{
		tempRoot:  tempRoot,
		objects:   objects,
		alternate: alternate,
	}
	snapshot, err := createSnapshot(ws, state.objectEnv())
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	state.pair = checkpointPair{projectOpen: snapshot, lastDiff: snapshot}
	return state, nil
}

func (s *transientCheckpoint) objectEnv() map[string]string {
	return map[string]string{
		"GIT_OBJECT_DIRECTORY":             s.objects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": s.alternate,
	}
}

func resolveWorkspace(workDir string) (workspace, error) {
	projectRoot, err := filepath.Abs(workDir)
	if err != nil {
		return workspace{}, err
	}
	projectRoot = filepath.Clean(projectRoot)
	out, err := gitChecked(projectRoot, []string{"rev-parse", "--show-toplevel"}, nil, nil)
	if err != nil {
		return workspace{}, fmt.Errorf("%w: %v", ErrNotGitWorktree, err)
	}
	gitRoot := filepath.Clean(strings.TrimSpace(string(out)))
	gitRoot, err = filepath.Abs(gitRoot)
	if err != nil {
		return workspace{}, err
	}
	relative, err := filepath.Rel(gitRoot, projectRoot)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return workspace{}, fmt.Errorf("project root %s is outside Git root %s", projectRoot, gitRoot)
	}
	pathspec := "."
	scope := "."
	if relative != "." && relative != "" {
		pathspec = filepath.ToSlash(relative)
		scope = pathspec
	}
	sum := sha256.Sum256([]byte("codexify-go/diff-workspace/v1\x00" + strings.ToLower(projectRoot)))
	return workspace{
		gitRoot:  gitRoot,
		pathspec: pathspec,
		scope:    scope,
		key:      hex.EncodeToString(sum[:]),
	}, nil
}

func createSnapshot(ws workspace, objectEnv map[string]string) (string, error) {
	tempDir, err := os.MkdirTemp("", "codexify-go-index-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tempDir)
	index := filepath.Join(tempDir, "index")

	scopedEntries, err := gitChecked(
		ws.gitRoot,
		[]string{"--literal-pathspecs", "ls-files", "--stage", "-z", "--", ws.pathspec},
		objectEnv,
		nil,
	)
	if err != nil {
		return "", err
	}

	env := cloneMap(objectEnv)
	env["GIT_INDEX_FILE"] = index
	env["GIT_AUTHOR_NAME"] = "Codexify Go Diff"
	env["GIT_AUTHOR_EMAIL"] = "diff@codexify-go.local"
	env["GIT_COMMITTER_NAME"] = "Codexify Go Diff"
	env["GIT_COMMITTER_EMAIL"] = "diff@codexify-go.local"
	env["GIT_AUTHOR_DATE"] = "@0 +0000"
	env["GIT_COMMITTER_DATE"] = "@0 +0000"

	if _, err := gitChecked(ws.gitRoot, []string{"read-tree", "--empty"}, env, nil); err != nil {
		return "", err
	}
	if len(scopedEntries) > 0 {
		if _, err := gitChecked(ws.gitRoot, []string{"update-index", "-z", "--index-info"}, env, scopedEntries); err != nil {
			return "", err
		}
	}
	add := runGit(ws.gitRoot, []string{"--literal-pathspecs", "add", "-A", "--", ws.pathspec}, env, nil, 0)
	if add.err != nil {
		if !(add.exitCode == 1 && strings.Contains(string(add.stderr), "paths are ignored")) {
			return "", addError("git add", add)
		}
	}

	tree, err := gitChecked(ws.gitRoot, []string{"write-tree"}, env, nil)
	if err != nil {
		return "", err
	}
	commit, err := gitChecked(ws.gitRoot, []string{"commit-tree", strings.TrimSpace(string(tree)), "-m", snapshotMessage}, env, nil)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(commit))
	if id == "" {
		return "", errors.New("Git returned an empty diff snapshot id")
	}
	return id, nil
}

func (m *Manager) compare(ws workspace, baseline, current string, req Request, objectEnv map[string]string) (Result, error) {
	nameStatus, err := gitChecked(ws.gitRoot, diffArgs("--name-status", baseline, current, ws.pathspec), objectEnv, nil)
	if err != nil {
		return Result{}, err
	}
	numstat, err := gitChecked(ws.gitRoot, diffArgs("--numstat", baseline, current, ws.pathspec), objectEnv, nil)
	if err != nil {
		return Result{}, err
	}
	names, err := parseNameStatus(nameStatus)
	if err != nil {
		return Result{}, err
	}
	stats, err := parseNumstat(numstat)
	if err != nil {
		return Result{}, err
	}
	files, err := mergeRecords(ws, names, stats)
	if err != nil {
		return Result{}, err
	}
	summary := summarize(files)

	result := Result{
		Since:            req.Since,
		AdvanceRequested: req.Advance,
		Scope:            ws.scope,
		Summary:          summary,
		Files:            files,
		Warnings:         []string{},
	}
	if !req.IncludePatch {
		result.PatchOmittedReason = "disabled by the show_diff request"
		return result, nil
	}
	if m.cfg.MaxPatchBytes == 0 {
		result.PatchOmittedReason = "disabled by diff.maxPatchBytes=0"
		return result, nil
	}

	args := []string{
		"--literal-pathspecs",
		"diff",
		"--no-ext-diff",
		"--no-textconv",
		"--diff-algorithm=histogram",
		"--find-renames",
		"--binary",
		"--full-index",
		"--no-color",
	}
	if ws.scope != "." {
		args = append(args, "--relative="+ws.scope)
	}
	args = append(args, baseline, current, "--", ws.pathspec)
	patchResult := runGit(ws.gitRoot, args, objectEnv, nil, m.cfg.MaxPatchBytes+1)
	if patchResult.err != nil {
		return Result{}, addError("git diff", patchResult)
	}
	if patchResult.truncated || len(patchResult.stdout) > m.cfg.MaxPatchBytes {
		result.PatchOmittedReason = fmt.Sprintf("exceeds diff.maxPatchBytes (%d bytes)", m.cfg.MaxPatchBytes)
		return result, nil
	}
	result.Patch = string(patchResult.stdout)
	result.PatchIncluded = true
	n := len(patchResult.stdout)
	result.PatchBytes = &n
	return result, nil
}

func diffArgs(kind, baseline, current, pathspec string) []string {
	return []string{
		"--literal-pathspecs",
		"diff",
		"--no-ext-diff",
		"--no-textconv",
		"--diff-algorithm=histogram",
		"--find-renames",
		kind,
		"-z",
		baseline,
		current,
		"--",
		pathspec,
	}
}

type nameRecord struct {
	status       string
	previousPath string
	path         string
}

type statRecord struct {
	additions    *uint64
	deletions    *uint64
	previousPath string
	path         string
}

func parseNameStatus(data []byte) ([]nameRecord, error) {
	tokens := nulTokens(data)
	var out []nameRecord
	for i := 0; i < len(tokens); {
		status := string(tokens[i])
		i++
		if status == "" {
			continue
		}
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if i+1 >= len(tokens) {
				return nil, errors.New("truncated Git rename/copy status")
			}
			out = append(out, nameRecord{
				status:       status,
				previousPath: string(tokens[i]),
				path:         string(tokens[i+1]),
			})
			i += 2
		} else {
			if i >= len(tokens) {
				return nil, errors.New("truncated Git name-status record")
			}
			out = append(out, nameRecord{status: status, path: string(tokens[i])})
			i++
		}
	}
	return out, nil
}

func parseNumstat(data []byte) ([]statRecord, error) {
	var out []statRecord
	cursor := 0
	for cursor < len(data) {
		header := takeNul(data, &cursor)
		if len(header) == 0 {
			continue
		}
		fields := bytes.SplitN(header, []byte{'\t'}, 3)
		if len(fields) != 3 {
			return nil, errors.New("invalid Git numstat record")
		}
		additions, err := parseStat(fields[0])
		if err != nil {
			return nil, err
		}
		deletions, err := parseStat(fields[1])
		if err != nil {
			return nil, err
		}
		if len(fields[2]) > 0 {
			out = append(out, statRecord{
				additions: additions,
				deletions: deletions,
				path:      string(fields[2]),
			})
			continue
		}
		previous := takeNul(data, &cursor)
		path := takeNul(data, &cursor)
		if len(previous) == 0 || len(path) == 0 {
			return nil, errors.New("truncated Git numstat rename record")
		}
		out = append(out, statRecord{
			additions:    additions,
			deletions:    deletions,
			previousPath: string(previous),
			path:         string(path),
		})
	}
	return out, nil
}

func parseStat(raw []byte) (*uint64, error) {
	if bytes.Equal(raw, []byte("-")) {
		return nil, nil
	}
	n, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid Git numstat value %q", raw)
	}
	return &n, nil
}

func mergeRecords(ws workspace, names []nameRecord, stats []statRecord) ([]File, error) {
	if len(names) != len(stats) {
		return nil, fmt.Errorf("Git returned %d name records but %d stat records", len(names), len(stats))
	}
	files := make([]File, 0, len(names))
	for i := range names {
		name := names[i]
		stat := stats[i]
		if name.path != stat.path || name.previousPath != stat.previousPath {
			return nil, errors.New("Git name-status and numstat records do not align")
		}
		path, err := projectRelative(ws, name.path)
		if err != nil {
			return nil, err
		}
		previous := ""
		if name.previousPath != "" {
			previous, err = projectRelative(ws, name.previousPath)
			if err != nil {
				return nil, err
			}
		}
		files = append(files, File{
			Path:         path,
			PreviousPath: previous,
			Status:       statusName(name.status),
			Additions:    stat.additions,
			Deletions:    stat.deletions,
			Binary:       stat.additions == nil || stat.deletions == nil,
		})
	}
	return files, nil
}

func projectRelative(ws workspace, repositoryPath string) (string, error) {
	repositoryPath = filepath.ToSlash(repositoryPath)
	if ws.scope == "." {
		return repositoryPath, nil
	}
	prefix := strings.TrimSuffix(ws.scope, "/") + "/"
	value, ok := strings.CutPrefix(repositoryPath, prefix)
	if !ok {
		return "", fmt.Errorf("Git returned a path outside logical diff scope: %s", repositoryPath)
	}
	return value, nil
}

func statusName(raw string) string {
	if raw == "" {
		return "unknown"
	}
	switch raw[0] {
	case 'A':
		return "added"
	case 'M':
		return "modified"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "copied"
	case 'T':
		return "type_changed"
	case 'U':
		return "unmerged"
	case 'X':
		return "unknown"
	case 'B':
		return "broken_pairing"
	default:
		return "unknown"
	}
}

func summarize(files []File) Summary {
	var out Summary
	out.Files = len(files)
	for _, file := range files {
		if file.Additions != nil {
			out.Additions += *file.Additions
		}
		if file.Deletions != nil {
			out.Deletions += *file.Deletions
		}
		if file.Binary {
			out.BinaryFiles++
		}
	}
	return out
}

func projectOpenRef(ws workspace, owner Owner) string {
	return fmt.Sprintf("%s/%s/%s/project-open", refRoot, shortRefKey(ws.key), shortRefKey(ownerKey(owner)))
}

func lastDiffRef(ws workspace, owner Owner) string {
	return fmt.Sprintf("%s/%s/%s/last-diff", refRoot, shortRefKey(ws.key), shortRefKey(ownerKey(owner)))
}

func shortRefKey(value string) string {
	if len(value) >= 24 {
		return value[:24]
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func ownerKey(owner Owner) string {
	raw := strings.TrimSpace(owner.Key)
	if raw == "" {
		return ""
	}
	validHex := len(raw) == 64
	if validHex {
		for _, r := range raw {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				validHex = false
				break
			}
		}
	}
	if validHex {
		return raw
	}
	sum := sha256.Sum256([]byte("codexify-go/diff-owner/v1\x00" + raw))
	return hex.EncodeToString(sum[:])
}

func transientKey(ws workspace, owner Owner) string {
	return ownerKey(owner) + ":" + ws.key
}

func readRef(gitRoot, ref string) (string, error) {
	result := runGit(gitRoot, []string{"rev-parse", "--verify", "--quiet", ref}, nil, nil, 4096)
	if result.err == nil {
		return strings.TrimSpace(string(result.stdout)), nil
	}
	if result.exitCode == 1 {
		return "", nil
	}
	return "", addError("git rev-parse "+ref, result)
}

func createRefIfAbsent(gitRoot, ref, value string) (bool, error) {
	input := []byte(fmt.Sprintf("create %s %s\n", ref, value))
	result := runGit(gitRoot, []string{"update-ref", "--stdin"}, nil, input, 4096)
	if result.err == nil {
		return true, nil
	}
	current, err := readRef(gitRoot, ref)
	if err != nil {
		return false, err
	}
	if current != "" {
		return false, nil
	}
	return false, addError("git update-ref create", result)
}

func compareAndSwapRef(gitRoot, ref, value, old string) (bool, error) {
	input := []byte(fmt.Sprintf("update %s %s %s\n", ref, value, old))
	result := runGit(gitRoot, []string{"update-ref", "--stdin"}, nil, input, 4096)
	if result.err == nil {
		return true, nil
	}
	current, err := readRef(gitRoot, ref)
	if err != nil {
		return false, err
	}
	if current != old {
		return false, nil
	}
	return false, addError("git update-ref compare-and-swap", result)
}

func gitChecked(root string, args []string, env map[string]string, stdin []byte) ([]byte, error) {
	result := runGit(root, args, env, stdin, 8<<20)
	if result.err != nil {
		return nil, addError("git "+strings.Join(args, " "), result)
	}
	return result.stdout, nil
}

type gitResult struct {
	stdout    []byte
	stderr    []byte
	exitCode  int
	truncated bool
	err       error
}

func runGit(root string, args []string, extraEnv map[string]string, stdin []byte, maxStdout int) gitResult {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = mergedEnv(extraEnv)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout boundedBuffer
	stdout.max = maxStdout
	var stderr boundedBuffer
	stderr.max = 64 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = -1
		}
	}
	return gitResult{
		stdout:    stdout.Bytes(),
		stderr:    stderr.Bytes(),
		exitCode:  code,
		truncated: stdout.truncated,
		err:       err,
	}
}

func addError(label string, result gitResult) error {
	message := strings.TrimSpace(string(result.stderr))
	if message == "" {
		message = strings.TrimSpace(string(result.stdout))
	}
	if message == "" {
		message = "Git command failed"
	}
	return fmt.Errorf("%s (exit %d): %s", label, result.exitCode, message)
}

type boundedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.max <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	remaining := b.max - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return original, nil
}

func (b *boundedBuffer) Bytes() []byte {
	return append([]byte(nil), b.buf.Bytes()...)
}

func mergedEnv(extra map[string]string) []string {
	base := os.Environ()
	if len(extra) == 0 {
		return append(base, "GIT_OPTIONAL_LOCKS=0")
	}
	keys := make(map[string]struct{}, len(extra)+1)
	keys[strings.ToUpper("GIT_OPTIONAL_LOCKS")] = struct{}{}
	for key := range extra {
		keys[strings.ToUpper(key)] = struct{}{}
	}
	out := make([]string, 0, len(base)+len(extra)+1)
	for _, entry := range base {
		key := entry
		if idx := strings.IndexByte(entry, '='); idx >= 0 {
			key = entry[:idx]
		}
		if _, remove := keys[strings.ToUpper(key)]; remove {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "GIT_OPTIONAL_LOCKS=0")
	for key, value := range extra {
		out = append(out, key+"="+value)
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+8)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func nulTokens(data []byte) [][]byte {
	raw := bytes.Split(data, []byte{0})
	out := make([][]byte, 0, len(raw))
	for _, token := range raw {
		if len(token) > 0 {
			out = append(out, token)
		}
	}
	return out
}

func takeNul(data []byte, cursor *int) []byte {
	if *cursor >= len(data) {
		return nil
	}
	next := bytes.IndexByte(data[*cursor:], 0)
	if next < 0 {
		token := data[*cursor:]
		*cursor = len(data)
		return token
	}
	end := *cursor + next
	token := data[*cursor:end]
	*cursor = end + 1
	return token
}
