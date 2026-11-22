// Package fswalk walks regular files beneath a directory, following directory
// and file symlinks while preventing recursive directory links.
package fswalk

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"starbound-modmanager/internal/i18n"
)

const maxDepth = 1024

// Files visits regular files beneath root. Paths passed to visit use the
// original path through any symlinks, while relative paths are rooted at root.
// Unresolvable symlinks are ignored and directory symlink loops are skipped.
func Files(ctx context.Context, root string, visit func(path, relative string, info fs.FileInfo) error) error {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() {
		return i18n.NewError(i18n.KeyErrFSNotDirectory, root)
	}
	return walk(ctx, root, "", []os.FileInfo{rootInfo}, visit)
}

func walk(ctx context.Context, directory, relative string, ancestors []os.FileInfo, visit func(path, relative string, info fs.FileInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			if unresolvableSymlink(path, entry) {
				continue
			}
			return i18n.WrapError(i18n.KeyErrFSAccess, err, path)
		}
		childRelative := entry.Name()
		if relative != "" {
			childRelative = filepath.Join(relative, entry.Name())
		}
		if info.IsDir() {
			cycle := false
			for _, ancestor := range ancestors {
				if os.SameFile(ancestor, info) {
					cycle = true
					break
				}
			}
			if cycle {
				continue
			}
			if len(ancestors) >= maxDepth {
				return i18n.NewError(i18n.KeyErrFSDepth, maxDepth, path)
			}
			nextAncestors := make([]os.FileInfo, len(ancestors)+1)
			copy(nextAncestors, ancestors)
			nextAncestors[len(ancestors)] = info
			if err := walk(ctx, path, childRelative, nextAncestors, visit); err != nil {
				return err
			}
			continue
		}
		if info.Mode().IsRegular() && visit != nil {
			if err := visit(path, childRelative, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func unresolvableSymlink(path string, entry os.DirEntry) bool {
	if entry.Type()&os.ModeSymlink != 0 {
		return true
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}
