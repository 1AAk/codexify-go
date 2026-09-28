package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Root struct {
	path string
	real string
}

func Canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	ancestor := abs
	var suffix []string
	for {
		if _, statErr := os.Lstat(ancestor); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}
	realAncestor, evalErr := filepath.EvalSymlinks(ancestor)
	if evalErr != nil {
		return "", evalErr
	}
	out := realAncestor
	for _, part := range suffix {
		out = filepath.Join(out, part)
	}
	return filepath.Clean(out), nil
}

func New(path string) (*Root, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("workspace root is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root is not a directory: %s", abs)
	}
	real, err := Canonical(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	real = filepath.Clean(real)
	return &Root{path: real, real: real}, nil
}

func (r *Root) Path() string { return r.path }

func (r *Root) Resolve(rel string, allowMissing bool) (string, error) {
	if rel == "" || rel == "." {
		return r.real, nil
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("path must be workspace-relative")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes workspace")
	}
	candidate := filepath.Join(r.real, clean)
	if err := r.ensureInside(candidate); err != nil {
		return "", err
	}

	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		if err := r.ensureInside(resolved); err != nil {
			return "", err
		}
		return resolved, nil
	}
	if !allowMissing || !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	ancestor := candidate
	var suffix []string
	for {
		if ancestor == r.real {
			break
		}
		if _, statErr := os.Lstat(ancestor); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("could not find existing workspace ancestor")
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}

	realAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	if err := r.ensureInside(realAncestor); err != nil {
		return "", err
	}
	resolved = realAncestor
	for _, part := range suffix {
		resolved = filepath.Join(resolved, part)
	}
	if err := r.ensureInside(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func (r *Root) Relative(abs string) (string, error) {
	if err := r.ensureInside(abs); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r.real, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return ".", nil
	}
	return filepath.ToSlash(rel), nil
}

func (r *Root) ensureInside(path string) error {
	rel, err := filepath.Rel(r.real, filepath.Clean(path))
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path escapes workspace: %s", path)
	}
	return nil
}
