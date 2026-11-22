package mods

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"starbound-modmanager/internal/fswalk"
	"starbound-modmanager/internal/i18n"
	"starbound-modmanager/internal/pak"
)

const disabledDirectoryPrefix = "_smm-disabled_"

// Mod is one discovered mod and its already-inspected assets.
type Mod struct {
	ID             string
	Name           string
	Version        string
	Path           string
	SourceType     string
	SourceLocation string
	OrderLocked    bool
	Available      bool
	Enabled        bool
	GameEnabled    bool
	ActivationPath string
	ActivationID   string
	Error          error
	Source         *pak.Archive
}

// Result contains mods discovered beneath a selected game or mod directory.
type Result struct {
	Root            string
	OutputDirectory string
	Mods            []Mod
	Warnings        []error
}

const (
	SourceLocationWorkshop = "workshop"
	SourceLocationLocal    = "local"
	SourceLocationSelected = "selected"
)

type candidate struct {
	path           string
	nameHint       string
	activationPath string
	disabled       bool
}

// Scan accepts a Starbound game root, a mods/workshop content folder, or one
// unpacked mod directory. If a game root is selected, its local mods and its
// adjacent Steam Workshop collection are both considered when present.
func Scan(ctx context.Context, selectedRoot string) (Result, error) {
	selectedRoot = strings.TrimSpace(selectedRoot)
	if selectedRoot == "" {
		return Result{}, i18n.NewError(i18n.KeyErrScanSourceRequired)
	}
	root, err := filepath.Abs(selectedRoot)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return Result{}, i18n.WrapError(i18n.KeyErrScanDirectoryAccess, err, root)
	}
	if !info.IsDir() {
		return Result{}, i18n.NewError(i18n.KeyErrScanNotDirectory, root)
	}

	containers, outputDirectory, singleMod, err := locateContainers(root)
	if err != nil {
		return Result{}, i18n.WrapError(i18n.KeyErrScanDirectoryAccess, err, root)
	}
	result := Result{Root: root, OutputDirectory: outputDirectory, Mods: make([]Mod, 0)}
	var candidates []candidate
	if singleMod {
		candidates, err = expandCandidate(ctx, root, filepath.Base(root))
		if err != nil {
			return Result{}, err
		}
		for i := range candidates {
			candidates[i].activationPath = root
			candidates[i].disabled = isDisabledActivation(root)
		}
	} else {
		seen := make(map[string]struct{})
		for _, container := range containers {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			entries, readErr := os.ReadDir(container)
			if readErr != nil {
				result.Warnings = append(result.Warnings, i18n.NewError(i18n.KeyWarnReadContainer, container, readErr))
				continue
			}
			sort.Slice(entries, func(i, j int) bool {
				return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
			})
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return Result{}, err
				}
				// Underscore-prefixed directories can be valid unpacked mods.
				if strings.HasPrefix(entry.Name(), ".") {
					continue
				}
				fullPath := filepath.Join(container, entry.Name())
				info, statErr := os.Stat(fullPath)
				if statErr != nil {
					if entry.Type()&os.ModeSymlink != 0 {
						result.Warnings = append(result.Warnings, i18n.NewError(i18n.KeyWarnReadSymlink, fullPath, statErr))
					}
					continue
				}
				var found []candidate
				var candidateErr error
				if info.IsDir() {
					found, candidateErr = expandCandidate(ctx, fullPath, entry.Name())
				} else if info.Mode().IsRegular() && isPakFile(entry.Name()) {
					found = []candidate{{path: fullPath, nameHint: modNameFromPath(fullPath)}}
				}
				if candidateErr != nil {
					result.Warnings = append(result.Warnings, i18n.NewError(i18n.KeyWarnReadCandidate, fullPath, candidateErr))
					continue
				}
				for _, item := range found {
					item.activationPath = fullPath
					item.disabled = isDisabledActivation(fullPath)
					absPath, absErr := filepath.Abs(item.path)
					if absErr != nil {
						result.Warnings = append(result.Warnings, i18n.NewError(i18n.KeyWarnResolvePath, item.path, absErr))
						continue
					}
					if _, exists := seen[absPath]; exists {
						continue
					}
					seen[absPath] = struct{}{}
					candidates = append(candidates, candidate{
						path:           absPath,
						nameHint:       item.nameHint,
						activationPath: item.activationPath,
						disabled:       item.disabled,
					})
				}
			}
		}
	}

	for _, item := range candidates {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		mod := inspectCandidate(ctx, item)
		result.Mods = append(result.Mods, mod)
		if !mod.Available && mod.Error != nil {
			result.Warnings = append(result.Warnings, i18n.NewError(i18n.KeyWarnReadCandidate, mod.Name, mod.Error))
		}
	}
	sort.SliceStable(result.Mods, func(i, j int) bool {
		left, right := strings.ToLower(result.Mods[i].Name), strings.ToLower(result.Mods[j].Name)
		if left == right {
			return strings.ToLower(result.Mods[i].Path) < strings.ToLower(result.Mods[j].Path)
		}
		return left < right
	})
	return result, nil
}

// InspectPath reads one mod path without scanning its parent directory. It is
// used for the manager's generated package when the selected source root is a
// Workshop folder and does not include the game's local mods directory.
func InspectPath(ctx context.Context, modPath string) Mod {
	absPath, err := filepath.Abs(modPath)
	if err != nil {
		absPath = modPath
	}
	return inspectCandidate(ctx, candidate{
		path:           absPath,
		nameHint:       modNameFromPath(absPath),
		activationPath: absPath,
		disabled:       isDisabledActivation(absPath),
	})
}

func locateContainers(root string) (containers []string, outputDirectory string, singleMod bool, err error) {
	base := strings.ToLower(filepath.Base(filepath.Clean(root)))
	parent := strings.ToLower(filepath.Base(filepath.Dir(root)))
	grandparent := strings.ToLower(filepath.Base(filepath.Dir(filepath.Dir(root))))
	if base == "mods" || (base == "211820" && parent == "content" && grandparent == "workshop") {
		return []string{root}, root, false, nil
	}
	if base == "content" && parent == "workshop" {
		workshopMods := filepath.Join(root, "211820")
		if isDirectory(workshopMods) {
			return []string{workshopMods}, workshopMods, false, nil
		}
	}

	addIfDirectory := func(candidate string) {
		abs, absErr := filepath.Abs(candidate)
		if absErr != nil || !isDirectory(abs) {
			return
		}
		for _, existing := range containers {
			if filepath.Clean(existing) == filepath.Clean(abs) {
				return
			}
		}
		containers = append(containers, abs)
	}
	localMods := filepath.Join(root, "mods")
	addIfDirectory(localMods)
	addIfDirectory(filepath.Join(root, "workshop", "content", "211820"))
	addIfDirectory(filepath.Join(root, "..", "..", "workshop", "content", "211820"))
	addIfDirectory(filepath.Join(root, "..", "workshop", "content", "211820"))
	if len(containers) > 0 {
		output := root
		if isDirectory(localMods) || isGameRoot(root) {
			output = localMods
		} else if strings.EqualFold(filepath.Base(containers[0]), "211820") {
			output = containers[0]
		}
		return containers, output, false, nil
	}
	if isGameRoot(root) {
		return nil, localMods, false, nil
	}
	if hasStarboundMetadata(root) || looksLikeAssetRoot(root) || parent == "mods" || parent == "211820" {
		return []string{root}, filepath.Dir(root), true, nil
	}

	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		return nil, "", false, readErr
	}
	hasChildMod := false
	hasDirectFile := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		entryInfo, statErr := os.Stat(filepath.Join(root, entry.Name()))
		if statErr != nil {
			continue
		}
		if entryInfo.IsDir() || (entryInfo.Mode().IsRegular() && isPakFile(entry.Name())) {
			hasChildMod = true
		}
		if entryInfo.Mode().IsRegular() && entry.Name() != "_metadata" && entry.Name() != ".metadata" {
			hasDirectFile = true
		}
	}
	if !hasChildMod && hasDirectFile {
		return []string{root}, filepath.Dir(root), true, nil
	}
	return []string{root}, root, false, nil
}

func expandCandidate(ctx context.Context, candidatePath, nameHint string) ([]candidate, error) {
	if !isDirectory(candidatePath) {
		return []candidate{{path: candidatePath, nameHint: nameHint}}, nil
	}
	var packs []string
	hasLooseAssets := false
	err := fswalk.Files(ctx, candidatePath, func(path, relative string, _ os.FileInfo) error {
		if relative == "_metadata" || relative == ".metadata" {
			return nil
		}
		if isPakFile(filepath.Base(path)) {
			packs = append(packs, path)
			return nil
		}
		hasLooseAssets = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(packs)
	if len(packs) > 0 {
		result := make([]candidate, 0, len(packs))
		for _, packPath := range packs {
			hint := nameHint
			if len(packs) > 1 {
				hint += " · " + modNameFromPath(packPath)
			}
			result = append(result, candidate{path: packPath, nameHint: hint})
		}
		return result, nil
	}
	if !hasLooseAssets {
		return nil, nil
	}
	return []candidate{{path: candidatePath, nameHint: nameHint}}, nil
}

func inspectCandidate(ctx context.Context, item candidate) Mod {
	activationPath := item.activationPath
	if activationPath == "" {
		activationPath = item.path
	}
	activationPath, err := filepath.Abs(activationPath)
	if err != nil {
		activationPath = item.activationPath
	}
	modPath, err := filepath.Abs(item.path)
	if err != nil {
		modPath = item.path
	}
	mod := Mod{
		ID:             logicalModID(modPath, activationPath),
		Name:           item.nameHint,
		Path:           modPath,
		SourceType:     "directory",
		SourceLocation: sourceLocation(modPath),
		OrderLocked:    hasProtectedWorkshopOrder(activationPath),
		GameEnabled:    !item.disabled,
		ActivationPath: activationPath,
		ActivationID:   logicalActivationID(activationPath),
	}
	if isPakFile(item.path) {
		mod.SourceType = "pak"
	}
	if mod.Name == "" {
		mod.Name = modNameFromPath(item.path)
	} else {
		mod.Name = strings.TrimPrefix(mod.Name, disabledDirectoryPrefix)
	}
	source, err := pak.Inspect(ctx, mod.Path)
	if err != nil {
		mod.Error = err
		return mod
	}
	if name, ok := metadataString(source.Metadata["friendlyName"]); ok && name != "" {
		mod.Name = name
	} else if name, ok := metadataString(source.Metadata["name"]); ok && name != "" {
		mod.Name = name
	}
	if version, ok := metadataString(source.Metadata["version"]); ok {
		mod.Version = version
	}
	mod.Source = source
	mod.Available = true
	mod.Enabled = false
	return mod
}

func hasProtectedWorkshopOrder(itemPath string) bool {
	itemPath = filepath.Clean(itemPath)
	for current := itemPath; ; current = filepath.Dir(current) {
		if strings.EqualFold(filepath.Base(current), "211820") &&
			strings.EqualFold(filepath.Base(filepath.Dir(current)), "content") &&
			strings.EqualFold(filepath.Base(filepath.Dir(filepath.Dir(current))), "workshop") {
			relative, err := filepath.Rel(current, itemPath)
			if err != nil || relative == "." {
				return false
			}
			itemID := strings.SplitN(relative, string(filepath.Separator), 2)[0]
			itemID = strings.TrimPrefix(strings.ToLower(itemID), strings.ToLower(disabledDirectoryPrefix))
			return itemID == "807695810" || itemID == "2240879531"
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return false
}

func sourceLocation(path string) string {
	path = filepath.Clean(path)
	for current := path; ; current = filepath.Dir(current) {
		if strings.EqualFold(filepath.Base(current), "211820") &&
			strings.EqualFold(filepath.Base(filepath.Dir(current)), "content") &&
			strings.EqualFold(filepath.Base(filepath.Dir(filepath.Dir(current))), "workshop") {
			return SourceLocationWorkshop
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	for current := path; ; current = filepath.Dir(current) {
		if strings.EqualFold(filepath.Base(current), "mods") {
			return SourceLocationLocal
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return SourceLocationSelected
}

func metadataString(value any) (string, bool) {
	if value == nil {
		return "", false
	}
	if text, ok := value.(string); ok {
		return text, true
	}
	return fmt.Sprint(value), true
}

func hasStarboundMetadata(directory string) bool {
	for _, name := range []string{"_metadata", ".metadata"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func looksLikeAssetRoot(directory string) bool {
	assetDirectories := map[string]struct{}{
		"ai": {}, "animations": {}, "biomes": {}, "cinematics": {}, "codex": {},
		"dungeons": {}, "effects": {}, "interface": {}, "items": {}, "liquids": {},
		"monsters": {}, "npcs": {}, "objects": {}, "parallax": {}, "particles": {},
		"player": {}, "projectiles": {}, "quests": {}, "scripts": {}, "ships": {},
		"spawners": {}, "species": {}, "stats": {}, "tech": {}, "tiles": {},
		"vehicles": {}, "weather": {}, "worldgen": {}, "worlds": {},
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		info, statErr := os.Stat(filepath.Join(directory, entry.Name()))
		if statErr == nil && info.IsDir() {
			if _, ok := assetDirectories[strings.ToLower(entry.Name())]; ok {
				return true
			}
		}
	}
	return false
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isGameRoot(root string) bool {
	if root == "" {
		return false
	}
	if hasGameInstallFiles(root) {
		return true
	}
	if strings.EqualFold(filepath.Base(root), "Starbound.app") {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func hasGameInstallFiles(root string) bool {
	if info, err := os.Stat(filepath.Join(root, "assets", "packed.pak")); err == nil && info.Mode().IsRegular() {
		return true
	}
	for _, executable := range []string{
		filepath.Join(root, "win64", "starbound.exe"),
		filepath.Join(root, "win32", "starbound.exe"),
		filepath.Join(root, "linux", "starbound"),
		filepath.Join(root, "osx", "Starbound.app"),
	} {
		if _, err := os.Stat(executable); err == nil {
			return true
		}
	}
	return false
}

// IsGameRoot reports whether directory contains a Starbound installation.
func IsGameRoot(directory string) bool {
	return isGameRoot(directory)
}

// GameRootFor finds a game installation at a selected directory or its mods folder.
func GameRootFor(directory string) string {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return ""
	}
	if strings.EqualFold(filepath.Base(abs), "Starbound.app") {
		for parent, depth := filepath.Dir(abs), 0; depth < 4; parent, depth = filepath.Dir(parent), depth+1 {
			if hasGameInstallFiles(parent) {
				return parent
			}
		}
		if isGameRoot(abs) {
			return abs
		}
	}
	if isGameRoot(abs) {
		return abs
	}
	if strings.EqualFold(filepath.Base(abs), "mods") {
		parent := filepath.Dir(abs)
		if isGameRoot(parent) {
			return parent
		}
	}
	return ""
}

func isPakFile(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".pak") || strings.HasSuffix(name, ".pak.bak")
}

func isDisabledPak(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".pak.bak")
}

func isDisabledDirectory(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), disabledDirectoryPrefix)
}

func isDisabledActivation(path string) bool {
	base := filepath.Base(filepath.Clean(path))
	return isDisabledDirectory(base) || isDisabledPak(base)
}

func logicalActivationID(path string) string {
	path = filepath.Clean(path)
	base := filepath.Base(path)
	directory := filepath.Dir(path)
	if isDisabledDirectory(base) {
		base = base[len(disabledDirectoryPrefix):]
		path = filepath.Join(directory, base)
	} else if isDisabledPak(base) {
		path = strings.TrimSuffix(path, filepath.Ext(path))
	}
	return filepath.Clean(path)
}

func logicalModID(path, activationPath string) string {
	activationPath = filepath.Clean(activationPath)
	path = filepath.Clean(path)
	relative, err := filepath.Rel(activationPath, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return logicalActivationID(path)
	}
	if isDisabledPak(relative) {
		relative = strings.TrimSuffix(relative, filepath.Ext(relative))
	}
	logicalActivation := logicalActivationID(activationPath)
	if relative == "." {
		return logicalActivation
	}
	return filepath.Clean(filepath.Join(logicalActivation, relative))
}

func modNameFromPath(path string) string {
	name := filepath.Base(path)
	if isDisabledPak(name) {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	if strings.EqualFold(filepath.Ext(name), ".pak") {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	name = strings.TrimPrefix(name, disabledDirectoryPrefix)
	return name
}
