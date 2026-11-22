package mods

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"starbound-modmanager/internal/i18n"
)

// SetGameEnabled moves all archives that share one Starbound load entry
// together, then returns records whose source paths point at the new location.
func SetGameEnabled(group []Mod, enabled bool) ([]Mod, error) {
	if len(group) == 0 {
		return nil, i18n.NewError(i18n.KeyErrActivationNotFound)
	}
	activationPath := filepath.Clean(group[0].ActivationPath)
	activationID := group[0].ActivationID
	for _, mod := range group {
		if filepath.Clean(mod.ActivationPath) != activationPath || mod.ActivationID != activationID {
			return nil, i18n.NewError(i18n.KeyErrActivationMixed)
		}
	}
	if group[0].GameEnabled == enabled {
		return append([]Mod(nil), group...), nil
	}

	info, err := os.Stat(activationPath)
	if err != nil {
		return nil, i18n.WrapError(i18n.KeyErrActivationAccess, err)
	}
	base := filepath.Base(activationPath)
	parent := filepath.Dir(activationPath)
	var target string
	if info.IsDir() {
		if enabled {
			if !isDisabledDirectory(base) || len(base) <= len(disabledDirectoryPrefix) {
				return nil, i18n.NewError(i18n.KeyErrActivationDirName)
			}
			target = filepath.Join(parent, base[len(disabledDirectoryPrefix):])
		} else {
			if isDisabledDirectory(base) {
				return nil, i18n.NewError(i18n.KeyErrActivationAlreadyDir)
			}
			target = filepath.Join(parent, disabledDirectoryPrefix+base)
		}
	} else {
		if !isPakFile(base) {
			return nil, i18n.NewError(i18n.KeyErrActivationUnsupported)
		}
		if enabled {
			if !isDisabledPak(base) {
				return nil, i18n.NewError(i18n.KeyErrActivationPakSuffix)
			}
			target = activationPath[:len(activationPath)-len(filepath.Ext(activationPath))]
		} else {
			if isDisabledPak(base) {
				return nil, i18n.NewError(i18n.KeyErrActivationAlreadyPak)
			}
			target = activationPath + ".bak"
		}
	}
	if _, err := os.Lstat(target); err == nil {
		return nil, i18n.NewError(i18n.KeyErrActivationTarget, target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	updated := make([]Mod, len(group))
	for i, mod := range group {
		relative, err := filepath.Rel(activationPath, mod.Path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, i18n.NewError(i18n.KeyErrActivationOutside, mod.Path)
		}
		nextPath := target
		if relative != "." {
			nextPath = filepath.Join(target, relative)
		}
		updated[i] = mod
		updated[i].Path = nextPath
		updated[i].ActivationPath = target
		updated[i].ActivationID = logicalActivationID(target)
		updated[i].ID = logicalModID(nextPath, target)
		updated[i].GameEnabled = enabled
		if mod.Source != nil {
			source := *mod.Source
			source.Path = nextPath
			if source.MetadataPath != "" {
				metadataRelative, metadataErr := filepath.Rel(activationPath, source.MetadataPath)
				if metadataErr == nil && metadataRelative != ".." && !strings.HasPrefix(metadataRelative, ".."+string(filepath.Separator)) {
					source.MetadataPath = filepath.Join(target, metadataRelative)
				}
			}
			updated[i].Source = &source
		}
	}

	if err := os.Rename(activationPath, target); err != nil {
		action := i18n.KeyActionDisable
		if enabled {
			action = i18n.KeyActionEnable
		}
		return nil, i18n.WrapError(i18n.KeyErrActivationToggle, err, i18n.Msg(action))
	}
	return updated, nil
}
