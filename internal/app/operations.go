package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"starbound-modmanager/internal/i18n"
	"starbound-modmanager/internal/mods"
	"starbound-modmanager/internal/pak"
)

type activationChange struct {
	id      string
	enabled bool
}

// CreateMergedMod installs the merged archive in the selected game's mods
// directory and disables the included source entries.
func (m *Manager) CreateMergedMod(ctx context.Context, metadataInput MergedModMetadata, onProgress func(MergeProgress)) (MergeReport, error) {
	outputFileName, err := normalizedMergedPackFileName(metadataInput.OutputFileName)
	if err != nil {
		return MergeReport{}, err
	}
	metadataInput.OutputFileName = outputFileName

	m.scanMu.Lock()
	defer m.scanMu.Unlock()

	m.mu.RLock()
	gameDir := m.gameDir
	stageDirectory := m.outDir
	currentPackFileName := storedMergedPackName(m.settings, gameDir)
	currentPackTracked := strings.TrimSpace(m.settings.MergedPackNames[gameDir]) != ""
	records := append([]mods.Mod(nil), m.records...)
	root := m.root
	m.mu.RUnlock()
	if gameDir == "" {
		return MergeReport{}, i18n.NewError(i18n.KeyErrGameRequired)
	}

	selectedGroups := make(map[string]bool)
	for _, record := range records {
		if !record.Enabled {
			continue
		}
		if !record.Available || record.Source == nil {
			return MergeReport{}, i18n.NewError(i18n.KeyErrMergeSourceUnreadable, record.Name)
		}
		selectedGroups[record.ActivationID] = true
	}
	if len(selectedGroups) == 0 {
		return MergeReport{}, i18n.NewError(i18n.KeyErrMergeNoSelection)
	}

	var sources []*pak.Archive
	for _, record := range records {
		if !selectedGroups[record.ActivationID] {
			continue
		}
		if !record.Available || record.Source == nil {
			return MergeReport{}, i18n.NewError(i18n.KeyErrMergeGroupUnreadable, record.Name)
		}
		sources = append(sources, record.Source)
	}
	if len(sources) == 0 {
		return MergeReport{}, i18n.NewError(i18n.KeyErrMergeNoSelection)
	}
	mergedPriority := effectiveArchivePriority(sources[0])
	for _, source := range sources[1:] {
		if priority := effectiveArchivePriority(source); priority > mergedPriority {
			mergedPriority = priority
		}
	}
	mergeSources := make([]*pak.Archive, len(sources))
	for i := range sources {
		mergeSources[len(sources)-1-i] = sources[i]
	}
	metadata, err := metadataInput.archiveMetadata(mergedPriority)
	if err != nil {
		return MergeReport{}, err
	}
	if err := os.MkdirAll(stageDirectory, 0o755); err != nil {
		return MergeReport{}, i18n.WrapError(i18n.KeyErrMergeStageDirectory, err)
	}
	stagePath := filepath.Join(stageDirectory, outputFileName)
	report, err := pak.Merge(ctx, mergeSources, stagePath, metadata, func(progress pak.Progress) {
		interval := progress.Total / 100
		if interval < 1 {
			interval = 1
		}
		if onProgress != nil && (progress.Current == 1 || progress.Current == progress.Total || progress.Current%interval == 0) {
			onProgress(MergeProgress{
				Phase:   "packing",
				Current: progress.Current,
				Total:   progress.Total,
				Path:    progress.Path,
			})
		}
	})
	if err != nil {
		return MergeReport{}, err
	}

	installDirectory := gameModsDirectory(gameDir)
	if err := os.MkdirAll(installDirectory, 0o755); err != nil {
		return MergeReport{}, i18n.WrapError(i18n.KeyErrMergeModsDirectory, err)
	}
	installPath := filepath.Join(installDirectory, outputFileName)
	currentInstallPath := filepath.Clean(filepath.Join(installDirectory, currentPackFileName))
	if _, statErr := os.Lstat(installPath + ".bak"); statErr == nil {
		return MergeReport{}, i18n.NewError(i18n.KeyErrMergeDisabledOutput, installPath+".bak")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return MergeReport{}, statErr
	}
	replacingTrackedOutput := false
	if info, statErr := os.Lstat(installPath); statErr == nil {
		if !info.Mode().IsRegular() {
			return MergeReport{}, i18n.NewError(i18n.KeyErrMergeOutputOccupied)
		}
		if !currentPackTracked || filepath.Clean(installPath) != currentInstallPath {
			return MergeReport{}, i18n.NewError(i18n.KeyErrMergeOutputOccupied)
		}
		if _, err := pak.Inspect(ctx, installPath); err != nil {
			return MergeReport{}, i18n.WrapError(i18n.KeyErrMergeRefuseOverwrite, err)
		}
		replacingTrackedOutput = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return MergeReport{}, statErr
	}

	outputActivationID := ""
	if replacingTrackedOutput {
		outputActivationID = filepath.Clean(installPath)
	}
	activationIDs := make([]string, 0, len(selectedGroups))
	for id := range selectedGroups {
		activationIDs = append(activationIDs, id)
	}
	sort.Strings(activationIDs)
	var changes []activationChange
	rollbackChanges := func() error { return m.rollbackActivationChanges(changes) }
	for _, id := range activationIDs {
		if id == outputActivationID {
			continue
		}
		previous, changed, err := m.setActivationGroup(id, false)
		if err != nil {
			rollbackErr := rollbackChanges()
			return MergeReport{}, joinRollbackError(err, rollbackErr)
		}
		if changed {
			changes = append(changes, activationChange{id: id, enabled: previous})
		}
	}

	if onProgress != nil {
		onProgress(MergeProgress{Phase: "installing", Current: report.AssetCount, Total: report.AssetCount, Path: installPath})
	}
	undoInstall, finishInstall, err := installMergedPack(stagePath, installPath)
	if err != nil {
		rollbackErr := rollbackChanges()
		return MergeReport{}, joinRollbackError(err, rollbackErr)
	}
	newOutput := mods.InspectPath(ctx, installPath)
	if !newOutput.Available || newOutput.Source == nil {
		undoErr := undoInstall()
		rollbackErr := rollbackChanges()
		reason := newOutput.Error
		if reason == nil {
			reason = i18n.NewError(i18n.KeyErrModUnreadable)
		}
		return MergeReport{}, joinRollbackError(i18n.WrapError(i18n.KeyErrMergeReadGenerated, reason), errors.Join(undoErr, rollbackErr))
	}
	newOutput.Enabled = false

	m.mu.Lock()
	previousPackFileName, previousPackFileNamePresent := m.settings.MergedPackNames[gameDir]
	previousSelection, previousSelectionPresent := m.settings.Enabled[root]
	previousSelection = cloneBoolMap(previousSelection)
	previousOrder, previousOrderPresent := m.settings.Orders[root]
	previousOrder = append([]string(nil), previousOrder...)
	nextRecords := append([]mods.Mod(nil), m.records...)
	for i := range nextRecords {
		nextRecords[i].Enabled = false
	}
	if outputIndex := recordIndexByPath(nextRecords, installPath); outputIndex >= 0 {
		nextRecords[outputIndex] = newOutput
	} else {
		nextRecords = append(nextRecords, newOutput)
	}
	if m.settings.MergedPackNames == nil {
		m.settings.MergedPackNames = make(map[string]string)
	}
	if m.settings.Orders == nil {
		m.settings.Orders = make(map[string][]string)
	}
	if m.settings.Enabled == nil {
		m.settings.Enabled = make(map[string]map[string]bool)
	}
	if m.settings.Enabled[root] == nil {
		m.settings.Enabled[root] = make(map[string]bool)
	}
	m.settings.MergedPackNames[gameDir] = outputFileName
	for _, record := range records {
		m.settings.Enabled[root][record.ID] = false
	}
	for _, record := range m.records {
		m.settings.Enabled[root][record.ID] = false
	}
	m.settings.Enabled[root][newOutput.ID] = false
	m.settings.Orders[root] = storedOrderIDs(nextRecords)
	persistErr := m.persistLocked()
	if persistErr != nil {
		if previousPackFileNamePresent {
			m.settings.MergedPackNames[gameDir] = previousPackFileName
		} else {
			delete(m.settings.MergedPackNames, gameDir)
		}
		if previousSelectionPresent {
			m.settings.Enabled[root] = previousSelection
		} else {
			delete(m.settings.Enabled, root)
		}
		if previousOrderPresent {
			m.settings.Orders[root] = previousOrder
		} else {
			delete(m.settings.Orders, root)
		}
	}
	m.mu.Unlock()
	if persistErr != nil {
		undoErr := undoInstall()
		rollbackErr := rollbackChanges()
		return MergeReport{}, joinRollbackError(persistErr, errors.Join(undoErr, rollbackErr))
	}
	_ = finishInstall()
	m.mu.Lock()
	m.records = nextRecords
	m.lockedCount = countLockedRecords(m.records)
	m.mu.Unlock()

	report.OutputPath = installPath
	if info, statErr := os.Stat(installPath); statErr == nil {
		report.SizeBytes = info.Size()
	}
	if onProgress != nil {
		onProgress(MergeProgress{Phase: "complete", Current: report.AssetCount, Total: report.AssetCount, Path: report.OutputPath})
	}
	return MergeReport{
		OutputPath:    report.OutputPath,
		AssetCount:    report.AssetCount,
		ConflictCount: report.ConflictCount,
		PatchTargets:  report.PatchTargets,
		PatchFiles:    report.PatchFiles,
		SizeBytes:     report.SizeBytes,
	}, nil
}

func (metadata MergedModMetadata) Validate() error {
	if _, err := normalizedMergedPackFileName(metadata.OutputFileName); err != nil {
		return err
	}
	_, err := metadata.archiveMetadata(0)
	return err
}

func normalizedMergedPackFileName(input string) (string, error) {
	name := strings.TrimSpace(input)
	if name == "" {
		return "", i18n.NewError(i18n.KeyErrFilenameRequired)
	}
	if strings.ContainsAny(name, `/\\`) {
		return "", i18n.NewError(i18n.KeyErrFilenamePath)
	}
	if !strings.EqualFold(filepath.Ext(name), ".pak") {
		if filepath.Ext(name) != "" {
			return "", i18n.NewError(i18n.KeyErrFilenameExtension)
		}
		name += ".pak"
	}
	if strings.HasPrefix(name, ".") {
		return "", i18n.NewError(i18n.KeyErrFilenameLeadingDot)
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if stem == "" || stem == "." || stem == ".." || strings.HasSuffix(stem, ".") || strings.HasSuffix(stem, " ") {
		return "", i18n.NewError(i18n.KeyErrFilenameInvalid)
	}
	for _, character := range name {
		if unicode.IsControl(character) || strings.ContainsRune(`<>:"/\\|?*`, character) {
			return "", i18n.NewError(i18n.KeyErrFilenameWindowsChars)
		}
	}
	deviceName := strings.ToUpper(strings.SplitN(stem, ".", 2)[0])
	if isWindowsDeviceName(deviceName) {
		return "", i18n.NewError(i18n.KeyErrFilenameReserved)
	}
	return name, nil
}

func isWindowsDeviceName(name string) bool {
	switch name {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(name) == 4 &&
		(strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) &&
		name[3] >= '1' && name[3] <= '9'
}

func (metadata MergedModMetadata) archiveMetadata(priority float64) (map[string]any, error) {
	name := strings.TrimSpace(metadata.Name)
	if name == "" {
		return nil, i18n.NewError(i18n.KeyErrMetadataNameRequired)
	}

	result := map[string]any{"name": name}
	for key, value := range map[string]string{
		"friendlyName": strings.TrimSpace(metadata.FriendlyName),
		"version":      strings.TrimSpace(metadata.Version),
		"author":       strings.TrimSpace(metadata.Author),
		"description":  strings.TrimSpace(metadata.Description),
	} {
		if value != "" {
			result[key] = value
		}
	}

	additionalJSON := strings.TrimSpace(metadata.AdditionalJSON)
	if additionalJSON != "" {
		var additional map[string]any
		if err := json.Unmarshal([]byte(additionalJSON), &additional); err != nil {
			return nil, i18n.WrapError(i18n.KeyErrExtraJSONInvalid, err)
		}
		if additional == nil {
			return nil, i18n.NewError(i18n.KeyErrExtraJSONNull)
		}
		for key, value := range additional {
			switch key {
			case "name", "friendlyName", "version", "author", "description", "priority":
				return nil, i18n.NewError(i18n.KeyErrExtraJSONReserved, key)
			}
			result[key] = value
		}
	}

	result["priority"] = priority
	return result, nil
}

func (m *Manager) setActivationGroup(activationID string, enabled bool) (previous bool, changed bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var group []mods.Mod
	for _, record := range m.records {
		if record.ActivationID == activationID {
			group = append(group, record)
		}
	}
	if len(group) == 0 {
		return false, false, i18n.NewError(i18n.KeyErrActivationMissing, activationID)
	}
	previous = group[0].GameEnabled
	for _, record := range group[1:] {
		if record.GameEnabled != previous {
			return false, false, i18n.NewError(i18n.KeyErrActivationInconsistent, activationID)
		}
	}
	if previous == enabled {
		return previous, false, nil
	}
	updated, err := mods.SetGameEnabled(group, enabled)
	if err != nil {
		return false, false, err
	}
	m.replaceGroupLocked(updated)
	return previous, true, nil
}

func (m *Manager) rollbackActivationChanges(changes []activationChange) error {
	var result error
	for i := len(changes) - 1; i >= 0; i-- {
		_, _, err := m.setActivationGroup(changes[i].id, changes[i].enabled)
		if err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	copy := make(map[string]bool, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func gameModsDirectory(gameDir string) string {
	if gameDir == "" {
		return ""
	}
	local := filepath.Join(gameDir, "mods")
	if info, err := os.Stat(local); err == nil && info.IsDir() {
		return local
	}
	parent := filepath.Join(filepath.Dir(gameDir), "mods")
	if info, err := os.Stat(parent); err == nil && info.IsDir() && filepath.Ext(strings.ToLower(gameDir)) == ".app" {
		return parent
	}
	return local
}

func joinRollbackError(operationErr, rollbackErr error) error {
	if rollbackErr == nil {
		return operationErr
	}
	return i18n.WrapErrorFormat(i18n.KeyErrRollback, errors.Join(operationErr, rollbackErr), operationErr, rollbackErr)
}

func installMergedPack(sourcePath, destinationPath string) (undo func() error, finish func() error, err error) {
	directory := filepath.Dir(destinationPath)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, nil, err
	}
	input, err := os.Open(sourcePath)
	if err != nil {
		return nil, nil, err
	}
	defer input.Close()

	temp, err := os.CreateTemp(directory, "."+filepath.Base(destinationPath)+"-*.tmp")
	if err != nil {
		return nil, nil, err
	}
	tempPath := temp.Name()
	cleanupTemp := func() {
		_ = os.Remove(tempPath)
	}
	if _, err := io.Copy(temp, input); err != nil {
		_ = temp.Close()
		cleanupTemp()
		return nil, nil, err
	}
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		cleanupTemp()
		return nil, nil, err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		cleanupTemp()
		return nil, nil, err
	}
	if err := temp.Close(); err != nil {
		cleanupTemp()
		return nil, nil, err
	}

	var backupPath string
	if _, err := os.Lstat(destinationPath); err == nil {
		backup, err := os.CreateTemp(directory, "."+filepath.Base(destinationPath)+"-backup-*")
		if err != nil {
			cleanupTemp()
			return nil, nil, err
		}
		backupPath = backup.Name()
		if err := backup.Close(); err != nil {
			_ = os.Remove(backupPath)
			cleanupTemp()
			return nil, nil, err
		}
		if err := os.Remove(backupPath); err != nil {
			cleanupTemp()
			return nil, nil, err
		}
		if err := os.Rename(destinationPath, backupPath); err != nil {
			cleanupTemp()
			return nil, nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanupTemp()
		return nil, nil, err
	}
	if err := os.Rename(tempPath, destinationPath); err != nil {
		if backupPath != "" {
			_ = os.Rename(backupPath, destinationPath)
		}
		cleanupTemp()
		return nil, nil, err
	}

	undo = func() error {
		if err := os.Remove(destinationPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if backupPath != "" {
			return os.Rename(backupPath, destinationPath)
		}
		return nil
	}
	finish = func() error {
		if backupPath == "" {
			return nil
		}
		return os.Remove(backupPath)
	}
	return undo, finish, nil
}
