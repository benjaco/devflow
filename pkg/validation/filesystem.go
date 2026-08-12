package validation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/benjaco/devflow/internal/pathspec"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/project"
)

type outputSpec struct {
	task string
	kind string
	path string
}

type fileState struct {
	kind    string
	mode    fs.FileMode
	size    int64
	modTime int64
	digest  string
}

type filesystemSnapshot map[string]fileState

func validateTaskPaths(task project.Task) []api.ValidationIssue {
	issues := make([]api.ValidationIssue, 0)
	check := func(kind, value string, output bool) {
		cleaned, err := cleanRelativePath(value)
		if err != nil {
			issues = append(issues, errorIssue("invalid_"+kind, task.Name, value, err.Error()))
			return
		}
		if isReservedValidationPath(cleaned) {
			issues = append(issues, errorIssue("reserved_"+kind, task.Name, value, fmt.Sprintf("%s %q points into .git or .devflow, which validation never copies", kind, value)))
			return
		}
		if output && cleaned == "" {
			issues = append(issues, errorIssue("broad_output", task.Name, value, "validation does not allow the worktree root as a declared output"))
		}
	}
	for _, value := range task.Inputs.Paths {
		check("input", value, false)
	}
	for _, value := range task.Inputs.Files {
		check("input", value, false)
	}
	for _, value := range task.Inputs.Dirs {
		check("input", value, false)
	}
	for _, value := range task.Inputs.Globs {
		check("input_glob", value, false)
	}
	for _, value := range task.Inputs.Filtered {
		check("filtered_input", value.Path, false)
	}
	for _, value := range task.Outputs.Paths {
		check("output", value, true)
	}
	for _, value := range task.Outputs.Files {
		check("output_file", value, true)
	}
	for _, value := range task.Outputs.Dirs {
		check("output_dir", value, true)
	}
	return issues
}

func outputCollisionIssues(g *graph.Graph, order []string) []api.ValidationIssue {
	specs := make([]outputSpec, 0)
	for _, name := range order {
		specs = append(specs, taskOutputSpecs(g.Tasks[name])...)
	}
	issues := make([]api.ValidationIssue, 0)
	seen := map[string]bool{}
	for i := 0; i < len(specs); i++ {
		for j := i + 1; j < len(specs); j++ {
			left, right := specs[i], specs[j]
			if left.task == right.task || !outputSpecsOverlap(left, right) {
				continue
			}
			keyParts := []string{left.task + ":" + left.path, right.task + ":" + right.path}
			sort.Strings(keyParts)
			key := strings.Join(keyParts, "|")
			if seen[key] {
				continue
			}
			seen[key] = true
			issues = append(issues, errorIssue(
				"output_collision",
				left.task,
				left.path,
				fmt.Sprintf("tasks %q and %q declare overlapping outputs %q and %q", left.task, right.task, left.path, right.path),
			))
		}
	}
	return issues
}

func taskOutputSpecs(task project.Task) []outputSpec {
	out := make([]outputSpec, 0, len(task.Outputs.Paths)+len(task.Outputs.Files)+len(task.Outputs.Dirs))
	seen := map[string]bool{}
	add := func(kind, value string) {
		cleaned, err := cleanRelativePath(value)
		if err != nil {
			return
		}
		key := kind + ":" + cleaned
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, outputSpec{task: task.Name, kind: kind, path: cleaned})
	}
	for _, value := range task.Outputs.Paths {
		add("path", value)
	}
	for _, value := range task.Outputs.Files {
		add("file", value)
	}
	for _, value := range task.Outputs.Dirs {
		add("dir", value)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].kind < out[j].kind
	})
	return out
}

func declaredOutputStrings(task project.Task) []string {
	specs := taskOutputSpecs(task)
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.kind+":"+spec.path)
	}
	return out
}

func declaredInputStrings(task project.Task) []string {
	out := make([]string, 0, len(task.Inputs.Paths)+len(task.Inputs.Files)+len(task.Inputs.Dirs)+len(task.Inputs.Globs)+len(task.Inputs.Filtered))
	add := func(kind, value string) {
		cleaned, err := cleanRelativePath(value)
		if err != nil {
			cleaned = value
		}
		out = append(out, kind+":"+cleaned)
	}
	for _, value := range task.Inputs.Paths {
		add("path", value)
	}
	for _, value := range task.Inputs.Files {
		add("file", value)
	}
	for _, value := range task.Inputs.Dirs {
		add("dir", value)
	}
	for _, value := range task.Inputs.Globs {
		add("glob", value)
	}
	for _, value := range task.Inputs.Filtered {
		add("filtered", value.Path)
	}
	sort.Strings(out)
	return out
}

func allOutputSpecs(g *graph.Graph, order []string) []outputSpec {
	out := make([]outputSpec, 0)
	for _, name := range order {
		out = append(out, taskOutputSpecs(g.Tasks[name])...)
	}
	return out
}

func outputSpecsOverlap(left, right outputSpec) bool {
	if left.path == right.path {
		return true
	}
	if left.kind != "file" && hasPathPrefix(right.path, left.path) {
		return true
	}
	return right.kind != "file" && hasPathPrefix(left.path, right.path)
}

func outputMatches(spec outputSpec, candidate string) bool {
	candidate = cleanSlash(candidate)
	if candidate == spec.path {
		return true
	}
	return spec.kind != "file" && hasPathPrefix(candidate, spec.path)
}

func anyOutputMatches(specs []outputSpec, candidate string) bool {
	for _, spec := range specs {
		if outputMatches(spec, candidate) {
			return true
		}
	}
	return false
}

func anyOutputRelated(specs []outputSpec, candidate string) bool {
	if anyOutputMatches(specs, candidate) {
		return true
	}
	for _, spec := range specs {
		if hasPathPrefix(spec.path, candidate) {
			return true
		}
	}
	return false
}

func cleanRelativePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	native := filepath.FromSlash(value)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return "", fmt.Errorf("path %q must be worktree-relative", value)
	}
	cleaned := cleanSlash(value)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes the worktree", value)
	}
	return cleaned, nil
}

func cleanSlash(value string) string {
	value = filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if value == "." {
		return ""
	}
	return strings.TrimPrefix(value, "./")
}

func isReservedValidationPath(value string) bool {
	for _, segment := range strings.Split(cleanSlash(value), "/") {
		if segment == ".git" || segment == ".devflow" {
			return true
		}
	}
	return false
}

func hasPathPrefix(candidate, parent string) bool {
	candidate = cleanSlash(candidate)
	parent = cleanSlash(parent)
	if parent == "" {
		return true
	}
	return strings.HasPrefix(candidate, parent+"/")
}

func clearSandbox(sandbox string) error {
	if err := os.RemoveAll(sandbox); err != nil {
		return err
	}
	return os.MkdirAll(sandbox, 0o755)
}

func materializeTaskInputs(ctx context.Context, sourceRoot, sandbox string, task project.Task) ([]string, error) {
	return materializeTaskInputsMatching(ctx, sourceRoot, sandbox, task, nil)
}

func materializeTaskInputsMatching(ctx context.Context, sourceRoot, sandbox string, task project.Task, accept func(string) bool) ([]string, error) {
	materialized := map[string]bool{}
	copyInput := func(value, inputBase string) error {
		cleaned, err := cleanRelativePath(value)
		if err != nil {
			return err
		}
		if accept != nil && !accept(cleaned) {
			return nil
		}
		full := filepath.Join(sourceRoot, filepath.FromSlash(cleaned))
		if _, err := os.Lstat(full); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		filter := func(rel string, info fs.FileInfo) bool {
			if isReservedValidationPath(rel) {
				return false
			}
			dirRelative := ""
			if inputBase != "" && rel != inputBase && hasPathPrefix(rel, inputBase) {
				dirRelative = strings.TrimPrefix(rel, inputBase+"/")
			}
			return !ignoredInputPath(task.Inputs.Ignore, rel, dirRelative)
		}
		return copyPathRecursive(ctx, sourceRoot, full, filepath.Join(sandbox, filepath.FromSlash(cleaned)), cleaned, filter, materialized, 0)
	}
	for _, value := range task.Inputs.Paths {
		if err := copyInput(value, cleanSlash(value)); err != nil {
			return nil, err
		}
	}
	for _, value := range task.Inputs.Files {
		if err := copyInput(value, ""); err != nil {
			return nil, err
		}
	}
	for _, value := range task.Inputs.Dirs {
		if err := copyInput(value, cleanSlash(value)); err != nil {
			return nil, err
		}
	}
	for _, pattern := range task.Inputs.Globs {
		matches, err := pathspec.ExpandGlob(sourceRoot, pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			if ignoredInputPath(task.Inputs.Ignore, match, "") {
				continue
			}
			if err := copyInput(match, ""); err != nil {
				return nil, err
			}
		}
	}
	for _, input := range task.Inputs.Filtered {
		if pathspec.HasGlob(input.Path) {
			matches, err := pathspec.ExpandGlob(sourceRoot, input.Path)
			if err != nil {
				return nil, err
			}
			for _, match := range matches {
				if ignoredInputPath(task.Inputs.Ignore, match, "") {
					continue
				}
				if err := copyInput(match, ""); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := copyInput(input.Path, cleanSlash(input.Path)); err != nil {
			return nil, err
		}
	}
	return sortedSet(materialized), nil
}

func ignoredInputPath(patterns []string, rootRelative, dirRelative string) bool {
	for _, patternValue := range patterns {
		if matchesInputIgnore(patternValue, rootRelative) || (dirRelative != "" && matchesInputIgnore(patternValue, dirRelative)) {
			return true
		}
	}
	return false
}

func matchesInputIgnore(patternValue, candidate string) bool {
	patternValue = cleanSlash(patternValue)
	candidate = cleanSlash(candidate)
	if ok, _ := path.Match(patternValue, candidate); ok {
		return true
	}
	return candidate == patternValue || hasPathPrefix(candidate, patternValue)
}

func copyWorktreeForOrders(ctx context.Context, sourceRoot, sandbox string, outputs []outputSpec) error {
	if err := clearSandbox(sandbox); err != nil {
		return err
	}
	entries, err := os.ReadDir(sourceRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		rel := cleanSlash(entry.Name())
		if isReservedValidationPath(rel) || anyOutputMatches(outputs, rel) {
			continue
		}
		filter := func(candidate string, _ fs.FileInfo) bool {
			return !isReservedValidationPath(candidate) && !anyOutputMatches(outputs, candidate)
		}
		if err := copyPathRecursive(ctx, sourceRoot, filepath.Join(sourceRoot, entry.Name()), filepath.Join(sandbox, entry.Name()), rel, filter, nil, 0); err != nil {
			return err
		}
	}
	return nil
}

func materializeInPlaceInputs(ctx context.Context, sourceRoot, sandbox string, g *graph.Graph, order []string) error {
	for _, name := range order {
		task := g.Tasks[name]
		taskOutputs := taskOutputSpecs(task)
		if len(taskOutputs) == 0 {
			continue
		}
		_, err := materializeTaskInputsMatching(ctx, sourceRoot, sandbox, task, func(input string) bool {
			return anyOutputRelated(taskOutputs, input)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func copyDirectoryContents(ctx context.Context, sourceRoot, destinationRoot string) ([]string, error) {
	materialized := map[string]bool{}
	entries, err := os.ReadDir(sourceRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		rel := cleanSlash(entry.Name())
		if err := copyPathRecursive(ctx, sourceRoot, filepath.Join(sourceRoot, entry.Name()), filepath.Join(destinationRoot, entry.Name()), rel, nil, materialized, 0); err != nil {
			return nil, err
		}
	}
	return sortedSet(materialized), nil
}

func copyPathRecursive(ctx context.Context, allowedRoot, source, destination, logicalRel string, include func(string, fs.FileInfo) bool, materialized map[string]bool, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return fmt.Errorf("symlink recursion exceeded at %q", logicalRel)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil {
			return fmt.Errorf("resolve symlink %q: %w", logicalRel, err)
		}
		inside, err := pathInsideRoot(allowedRoot, resolved)
		if err != nil {
			return err
		}
		if !inside {
			return fmt.Errorf("symlink %q resolves outside the worktree; validation will not copy external targets", logicalRel)
		}
		return copyPathRecursive(ctx, allowedRoot, resolved, destination, logicalRel, include, materialized, depth+1)
	}
	if include != nil && !include(logicalRel, info) {
		return nil
	}
	if info.IsDir() {
		if existing, err := os.Lstat(destination); err == nil && !existing.IsDir() {
			if err := os.RemoveAll(destination); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			childRel := cleanSlash(path.Join(logicalRel, entry.Name()))
			if err := copyPathRecursive(ctx, allowedRoot, filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()), childRel, include, materialized, depth); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("validation cannot copy special file %q (%s)", logicalRel, info.Mode())
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if existing, err := os.Lstat(destination); err == nil && existing.IsDir() {
		if err := os.RemoveAll(destination); err != nil {
			return err
		}
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	_ = os.Chtimes(destination, info.ModTime(), info.ModTime())
	if materialized != nil {
		materialized[logicalRel] = true
	}
	return nil
}

func pathInsideRoot(root, candidate string) (bool, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return false, err
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

func validateSandboxSymlinks(root string) error {
	return filepath.WalkDir(root, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		target, err := os.Readlink(full)
		if err != nil {
			return err
		}
		candidate := target
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(filepath.Dir(full), candidate)
		}
		if resolved, resolveErr := filepath.EvalSymlinks(full); resolveErr == nil {
			candidate = resolved
		}
		inside, err := pathInsideRoot(root, candidate)
		if err != nil {
			return err
		}
		if !inside {
			return fmt.Errorf("symlink %q resolves outside the validation worktree", filepath.ToSlash(rel))
		}
		return nil
	})
}

func snapshotFilesystem(root string, includeModTime bool) (filesystemSnapshot, error) {
	snapshot := filesystemSnapshot{}
	err := filepath.WalkDir(root, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = cleanSlash(rel)
		if rel == "" {
			return nil
		}
		if isReservedValidationPath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		state := fileState{mode: info.Mode().Perm(), size: info.Size()}
		if includeModTime {
			state.modTime = info.ModTime().UnixNano()
		}
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			state.kind = "symlink"
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			state.digest = hashBytes([]byte(target))
		case entry.IsDir():
			state.kind = "dir"
		default:
			state.kind = "file"
			digest, err := hashFile(full)
			if err != nil {
				return err
			}
			state.digest = digest
		}
		snapshot[rel] = state
		return nil
	})
	return snapshot, err
}

func changedFiles(before, after filesystemSnapshot) []string {
	set := map[string]bool{}
	for rel, oldState := range before {
		newState, ok := after[rel]
		if ok && oldState == newState {
			continue
		}
		if ok && oldState.kind == "dir" && newState.kind == "dir" && oldState.mode == newState.mode {
			continue
		}
		set[rel] = true
	}
	for rel, newState := range after {
		oldState, ok := before[rel]
		if ok && oldState == newState {
			continue
		}
		if ok && oldState.kind == "dir" && newState.kind == "dir" && oldState.mode == newState.mode {
			continue
		}
		set[rel] = true
	}
	return sortedSet(set)
}

func matchingSnapshotPaths(snapshot filesystemSnapshot, specs []outputSpec) []string {
	set := map[string]bool{}
	for rel := range snapshot {
		if anyOutputMatches(specs, rel) {
			set[rel] = true
		}
	}
	return sortedSet(set)
}

func missingDeclaredOutputs(worktree string, task project.Task) ([]string, error) {
	missing := make([]string, 0)
	for _, spec := range taskOutputSpecs(task) {
		full := filepath.Join(worktree, filepath.FromSlash(spec.path))
		info, err := os.Stat(full)
		if err != nil {
			if os.IsNotExist(err) {
				missing = append(missing, spec.kind+":"+spec.path)
				continue
			}
			return nil, err
		}
		if spec.kind == "file" && info.IsDir() {
			missing = append(missing, spec.kind+":"+spec.path)
		}
		if spec.kind == "dir" && !info.IsDir() {
			missing = append(missing, spec.kind+":"+spec.path)
		}
	}
	return missing, nil
}

func archiveTaskOutputs(ctx context.Context, worktree, archiveRoot string, task project.Task) error {
	if err := os.RemoveAll(archiveRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		return err
	}
	for _, spec := range taskOutputSpecs(task) {
		source := filepath.Join(worktree, filepath.FromSlash(spec.path))
		if _, err := os.Lstat(source); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := copyPathRecursive(ctx, worktree, source, filepath.Join(archiveRoot, filepath.FromSlash(spec.path)), spec.path, nil, nil, 0); err != nil {
			return err
		}
	}
	return nil
}

func outputSnapshot(worktree string, specs []outputSpec) (filesystemSnapshot, []string, error) {
	all, err := snapshotFilesystem(worktree, false)
	if err != nil {
		return nil, nil, err
	}
	filtered := filesystemSnapshot{}
	for rel, state := range all {
		if anyOutputMatches(specs, rel) {
			filtered[rel] = state
		}
	}
	missing := make([]string, 0)
	for _, spec := range specs {
		full := filepath.Join(worktree, filepath.FromSlash(spec.path))
		info, statErr := os.Stat(full)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				missing = append(missing, spec.kind+":"+spec.path)
				continue
			}
			return nil, nil, statErr
		}
		if (spec.kind == "file" && info.IsDir()) || (spec.kind == "dir" && !info.IsDir()) {
			missing = append(missing, spec.kind+":"+spec.path)
		}
	}
	sort.Strings(missing)
	return filtered, missing, nil
}

func snapshotDigest(snapshot filesystemSnapshot) string {
	keys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, key := range keys {
		state := snapshot[key]
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%s\x00", key, state.kind, state.mode.String(), state.size, state.digest)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func snapshotDifferences(left, right filesystemSnapshot) []string {
	set := map[string]bool{}
	for rel, leftState := range left {
		if rightState, ok := right[rel]; !ok || rightState != leftState {
			set[rel] = true
		}
	}
	for rel, rightState := range right {
		if leftState, ok := left[rel]; !ok || leftState != rightState {
			set[rel] = true
		}
	}
	return sortedSet(set)
}

func hashFile(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
