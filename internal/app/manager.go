package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"starbound-modmanager/internal/i18n"
	"starbound-modmanager/internal/mods"
	"starbound-modmanager/internal/pak"
)

const (
	defaultGeneratedPackName = "Starbound-ModManager-Merged.pak"
	settingsFileName         = "Starbound-ModManager.json"
)

// MergeProgress is sent to the active window while the output archive is written.
type MergeProgress struct {
	Phase   string `json:"phase"`
	Current int    `json:"current"`
	Total   int    `json:"total"`
	Path    string `json:"path"`
}

// ModInfo is the part of a discovered mod shown in the native UI.
type ModInfo struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Version        string  `json:"version"`
	Path           string  `json:"path"`
	SourceType     string  `json:"sourceType"`
	SourceLocation string  `json:"sourceLocation"`
	OrderLocked    bool    `json:"orderLocked"`
	Available      bool    `json:"available"`
	Enabled        bool    `json:"enabled"`
	GameEnabled    bool    `json:"gameEnabled"`
	Error          error   `json:"-"`
	AssetCount     int     `json:"assetCount"`
	PatchCount     int     `json:"patchCount"`
	ConflictCount  int     `json:"conflictCount"`
	PriorityNumber int     `json:"priorityNumber"`
	Priority       float64 `json:"priority"`
	PriorityKnown  bool    `json:"priorityKnown"`
}

// ConflictInfo describes a normal asset path provided by more than one active mod.
type ConflictInfo struct {
	Path   string   `json:"path"`
	Winner string   `json:"winner"`
	Mods   []string `json:"mods"`
}

// AppState is the complete view model used by the native UI.
type AppState struct {
	Language               string         `json:"language"`
	Root                   string         `json:"root"`
	GameDirectory          string         `json:"gameDirectory"`
	InstallDirectory       string         `json:"installDirectory"`
	MergedPackFileName     string         `json:"mergedPackFileName"`
	Mods                   []ModInfo      `json:"mods"`
	Conflicts              []ConflictInfo `json:"conflicts"`
	Warnings               []error        `json:"-"`
	EnabledCount           int            `json:"enabledCount"`
	AssetCount             int            `json:"assetCount"`
	ConflictCount          int            `json:"conflictCount"`
	PatchTargetCount       int            `json:"patchTargetCount"`
	PatchFileCount         int            `json:"patchFileCount"`
	BaseAssetPriority      float64        `json:"baseAssetPriority"`
	BaseAssetPriorityKnown bool           `json:"baseAssetPriorityKnown"`
	CanMerge               bool           `json:"canMerge"`
	PriorityNeedsApply     bool           `json:"priorityNeedsApply"`
}

// MergeReport is returned after an output .pak has been written.
type MergeReport struct {
	OutputPath    string `json:"outputPath"`
	AssetCount    int    `json:"assetCount"`
	ConflictCount int    `json:"conflictCount"`
	PatchTargets  int    `json:"patchTargets"`
	PatchFiles    int    `json:"patchFiles"`
	SizeBytes     int64  `json:"sizeBytes"`
}

// MergedModMetadata contains editable metadata for the generated archive.
// AdditionalJSON accepts an optional JSON object with extra Starbound fields.
type MergedModMetadata struct {
	OutputFileName string
	Name           string
	FriendlyName   string
	Version        string
	Author         string
	Description    string
	AdditionalJSON string
}

// DefaultMergedModMetadata returns the values initially shown before a merge.
func DefaultMergedModMetadata(sourceCount int) MergedModMetadata {
	return MergedModMetadata{
		OutputFileName: defaultGeneratedPackName,
		Name:           "Starbound-ModManager-Merged",
		FriendlyName:   "Starbound Mod Manager Merged",
		Version:        "1.0.0",
		Author:         "Starbound Mod Manager",
		Description:    fmt.Sprintf("Merged mod generated from %d selected mods.", sourceCount),
	}
}

type storedSettings struct {
	Language             string                     `json:"language"`
	Root                 string                     `json:"root"`
	GameDirectory        string                     `json:"gameDirectory"`
	MergedPackNames      map[string]string          `json:"mergedPackNames"`
	Orders               map[string][]string        `json:"orders"`
	Enabled              map[string]map[string]bool `json:"enabled"`
}

// Manager owns mod discovery, priority settings, and merged archive creation.
type Manager struct {
	mu                     sync.RWMutex
	scanMu                 sync.Mutex
	settings               storedSettings
	filePath               string
	root                   string
	gameDir                string
	outDir                 string
	records                []mods.Mod
	lockedCount            int
	warnings               []error
	baseAssetPriority      float64
	baseAssetPriorityKnown bool
}

// NewManager loads portable settings from the current working directory.
func NewManager() (*Manager, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return nil, i18n.WrapError(i18n.KeyErrWorkingDirectory, err)
	}
	filePath := filepath.Join(workingDirectory, settingsFileName)
	manager := &Manager{
		filePath: filePath,
		outDir:   filepath.Join(os.TempDir(), "Starbound-ModManager", "output"),
		settings: storedSettings{
			Language:        string(i18n.Chinese),
			MergedPackNames: make(map[string]string),
			Orders:          make(map[string][]string),
			Enabled:         make(map[string]map[string]bool),
		},
	}
	data, err := os.ReadFile(filePath)
	createLocalSettings := errors.Is(err, os.ErrNotExist)
	discardLegacyMergeState := false
	if createLocalSettings {
		if configDirectory, configErr := os.UserConfigDir(); configErr == nil {
			legacyPath := filepath.Join(configDirectory, "Starbound-ModManager", "settings.json")
			if legacyData, legacyErr := os.ReadFile(legacyPath); legacyErr == nil {
				data = legacyData
				err = nil
			} else if !errors.Is(legacyErr, os.ErrNotExist) {
				manager.warnings = append(manager.warnings, i18n.NewError(i18n.KeyWarnLegacySettings, legacyErr))
			}
		}
	}
	if err == nil {
		if decodeErr := json.Unmarshal(data, &manager.settings); decodeErr != nil {
			manager.warnings = append(manager.warnings, i18n.NewError(i18n.KeyWarnDecodeSettings, decodeErr))
		} else {
			var legacySettings map[string]json.RawMessage
			if json.Unmarshal(data, &legacySettings) == nil {
				_, discardLegacyMergeState = legacySettings["mergedOriginalStates"]
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		manager.warnings = append(manager.warnings, i18n.NewError(i18n.KeyWarnReadSettings, err))
	}
	if manager.settings.Orders == nil {
		manager.settings.Orders = make(map[string][]string)
	}
	if manager.settings.MergedPackNames == nil {
		manager.settings.MergedPackNames = make(map[string]string)
	}
	if manager.settings.Enabled == nil {
		manager.settings.Enabled = make(map[string]map[string]bool)
	}
	if manager.settings.Language != "" && !i18n.IsSupported(manager.settings.Language) {
		manager.warnings = append(manager.warnings, i18n.NewError(i18n.KeyErrLanguageUnsupported, manager.settings.Language))
	}
	manager.settings.Language = string(i18n.Normalize(manager.settings.Language))
	manager.root = manager.settings.Root
	manager.gameDir = manager.settings.GameDirectory
	if manager.gameDir == "" {
		manager.gameDir = mods.GameRootFor(manager.root)
	}
	if createLocalSettings || discardLegacyMergeState {
		if err := manager.persistLocked(); err != nil {
			manager.warnings = append(manager.warnings, i18n.NewError(i18n.KeyWarnSaveSettings, err))
		}
	}
	return manager, nil
}

// SetLanguage saves the interface locale in the portable settings file.
func (m *Manager) SetLanguage(locale i18n.Locale) error {
	if !i18n.IsSupported(string(locale)) {
		return i18n.NewError(i18n.KeyErrLanguageUnsupported, string(locale))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.settings.Language
	m.settings.Language = string(locale)
	if err := m.persistLocked(); err != nil {
		m.settings.Language = previous
		return i18n.WrapError(i18n.KeyWarnSaveSettings, err)
	}
	return nil
}

// GetState returns the current mod list and the last selected root path.
func (m *Manager) GetState() AppState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.buildStateLocked()
}

// Scan discovers local and Workshop mods beneath root, restores the saved
// display order, and refreshes conflicts without modifying mod files.
func (m *Manager) Scan(ctx context.Context, root string) (AppState, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()

	result, err := mods.Scan(ctx, root)
	if err != nil {
		return AppState{}, err
	}

	detectedGameDir := mods.GameRootFor(result.Root)
	m.mu.RLock()
	gameDir := m.gameDir
	m.mu.RUnlock()
	if detectedGameDir != "" {
		gameDir = detectedGameDir
	}
	m.mu.RLock()
	mergedPackFileName := storedMergedPackName(m.settings, gameDir)
	mergedPackTracked := strings.TrimSpace(m.settings.MergedPackNames[gameDir]) != ""
	m.mu.RUnlock()
	if gameDir != "" && mergedPackTracked {
		activeMergedPackPath := filepath.Clean(filepath.Join(gameModsDirectory(gameDir), mergedPackFileName))
		foundMergedPack := false
		for _, record := range result.Mods {
			if filepath.Clean(record.ActivationID) == activeMergedPackPath {
				foundMergedPack = true
				break
			}
		}
		if !foundMergedPack {
			for _, candidatePath := range []string{activeMergedPackPath, activeMergedPackPath + ".bak"} {
				if info, statErr := os.Stat(candidatePath); statErr == nil && info.Mode().IsRegular() {
					result.Mods = append(result.Mods, mods.InspectPath(ctx, candidatePath))
					break
				}
			}
		}
	}
	basePriority, basePriorityKnown, basePriorityWarning := readBaseAssetPriority(ctx, gameDir)

	m.mu.Lock()
	m.root = result.Root
	if detectedGameDir != "" {
		m.gameDir = detectedGameDir
		m.settings.GameDirectory = detectedGameDir
	}
	if gameDir != "" && storedMergedPackName(m.settings, gameDir) != mergedPackFileName {
		m.settings.MergedPackNames[gameDir] = mergedPackFileName
	}
	m.baseAssetPriority = basePriority
	m.baseAssetPriorityKnown = basePriorityKnown
	m.records = append([]mods.Mod(nil), result.Mods...)
	m.warnings = result.Warnings
	if saved := m.settings.Enabled[m.root]; saved != nil {
		for i := range m.records {
			if enabled, exists := saved[m.records[i].ID]; exists {
				m.records[i].Enabled = enabled && m.records[i].Available
			}
		}
	}
	m.applySavedGroupSelectionLocked()
	saved := m.settings.Enabled[m.root]
	if saved == nil {
		saved = make(map[string]bool)
		m.settings.Enabled[m.root] = saved
	}
	sortRecordsByPriority(m.records)
	m.lockedCount = countLockedRecords(m.records)
	if len(m.settings.Orders[m.root]) > 0 {
		m.applySavedOrderLocked()
	}
	m.settings.Root = m.root
	if basePriorityWarning != nil {
		m.warnings = append(m.warnings, basePriorityWarning)
	}
	if err := m.persistLocked(); err != nil {
		m.warnings = append(m.warnings, i18n.NewError(i18n.KeyWarnSaveCurrentSettings, err))
	}
	state := m.buildStateLocked()
	m.mu.Unlock()
	return state, nil
}

// SetGameDirectory stores the Starbound installation used for installation and direct launch.
func (m *Manager) SetGameDirectory(directory string) (AppState, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return AppState{}, i18n.NewError(i18n.KeyErrGameRequired)
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return AppState{}, err
	}
	gameRoot := mods.GameRootFor(abs)
	if gameRoot == "" {
		return AppState{}, i18n.NewError(i18n.KeyErrGameDirectoryInvalid)
	}
	m.scanMu.Lock()
	defer m.scanMu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.gameDir
	previousBasePriority := m.baseAssetPriority
	previousBasePriorityKnown := m.baseAssetPriorityKnown
	m.gameDir = gameRoot
	m.settings.GameDirectory = gameRoot
	if previous != gameRoot {
		m.baseAssetPriority = 0
		m.baseAssetPriorityKnown = false
	}
	if err := m.persistLocked(); err != nil {
		m.gameDir = previous
		m.settings.GameDirectory = previous
		m.baseAssetPriority = previousBasePriority
		m.baseAssetPriorityKnown = previousBasePriorityKnown
		return m.buildStateLocked(), err
	}
	return m.buildStateLocked(), nil
}

// SetOrder applies the displayed load sequence to ordinary mod priorities.
// The first listed ordinary mod loads first; lower rows receive increasing
// priority values. Protected Workshop translations retain their existing
// priority.
func (m *Manager) SetOrder(ctx context.Context, ids []string) (AppState, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.mu.Lock()
	ordered, orderErr := m.orderRecordsLocked(ids)
	if orderErr != nil {
		state := m.buildStateLocked()
		m.mu.Unlock()
		return state, orderErr
	}
	gameDir := m.gameDir
	basePriority := m.baseAssetPriority
	basePriorityKnown := m.baseAssetPriorityKnown
	root := m.root
	m.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	var basePriorityWarning error
	if !basePriorityKnown {
		basePriority, basePriorityKnown, basePriorityWarning = readBaseAssetPriority(ctx, gameDir)
	}
	updated, priorityErr := writeOrderedPriorities(ctx, ordered)
	if priorityErr != nil {
		m.mu.Lock()
		m.replaceGroupLocked(updated)
		if basePriorityKnown {
			m.baseAssetPriority = basePriority
			m.baseAssetPriorityKnown = true
		}
		if basePriorityWarning != nil {
			m.warnings = append(m.warnings, basePriorityWarning)
		}
		state := m.buildStateLocked()
		m.mu.Unlock()
		return state, priorityErr
	}

	m.mu.Lock()
	m.records = updated
	m.settings.Orders[root] = storedOrderIDs(ordered)
	if basePriorityKnown {
		m.baseAssetPriority = basePriority
		m.baseAssetPriorityKnown = true
	}
	if basePriorityWarning != nil {
		m.warnings = append(m.warnings, basePriorityWarning)
	}
	if err := m.persistLocked(); err != nil {
		m.warnings = append(m.warnings, i18n.NewError(i18n.KeyWarnSaveOrderSettings, err))
	}
	state := m.buildStateLocked()
	m.mu.Unlock()
	return state, nil
}

// PreviewOrder updates the in-memory order and conflict preview without
// changing mod metadata or saving the order.
func (m *Manager) PreviewOrder(ids []string) (AppState, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	ordered, err := m.orderRecordsLocked(ids)
	if err != nil {
		return m.buildStateLocked(), err
	}
	m.records = ordered
	return m.buildStateLocked(), nil
}

func (m *Manager) orderRecordsLocked(ids []string) ([]mods.Mod, error) {
	if len(ids) != len(m.records) {
		return nil, i18n.NewError(i18n.KeyErrOrderMismatch)
	}
	byID := make(map[string]mods.Mod, len(m.records))
	for _, record := range m.records {
		byID[record.ID] = record
	}
	ordered := make([]mods.Mod, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		record, ok := byID[id]
		if !ok {
			return nil, i18n.NewError(i18n.KeyErrOrderUnknownMod, id)
		}
		if _, exists := seen[id]; exists {
			return nil, i18n.NewError(i18n.KeyErrOrderDuplicate)
		}
		seen[id] = struct{}{}
		ordered = append(ordered, record)
	}
	if !lockedGroupAtFront(ordered, m.lockedCount) {
		return nil, i18n.NewError(i18n.KeyErrOrderProtected)
	}
	return ordered, nil
}

func countLockedRecords(records []mods.Mod) int {
	count := 0
	for _, record := range records {
		if record.OrderLocked {
			count++
		}
	}
	return count
}

func storedOrderIDs(records []mods.Mod) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		if !record.OrderLocked {
			ids = append(ids, record.ID)
		}
	}
	return ids
}

func lockedGroupAtFront(records []mods.Mod, count int) bool {
	if count < 0 || count > len(records) {
		return false
	}
	for i, record := range records {
		if record.OrderLocked != (i < count) {
			return false
		}
	}
	return true
}

func effectiveModPriority(record mods.Mod) float64 {
	return effectiveArchivePriority(record.Source)
}

func effectiveArchivePriority(archive *pak.Archive) float64 {
	if priority, exists := metadataPriority(archive); exists {
		return priority
	}
	return 0
}

// SetIncluded toggles whether a mod is included in the merged package.
func (m *Manager) SetIncluded(id string, included bool) (AppState, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	previousRecords := append([]mods.Mod(nil), m.records...)
	previousSelection, hadPreviousSelection := m.settings.Enabled[m.root]
	previousSelection = cloneBoolMap(previousSelection)
	rollback := func(err error) (AppState, error) {
		m.records = previousRecords
		if hadPreviousSelection {
			m.settings.Enabled[m.root] = previousSelection
		} else {
			delete(m.settings.Enabled, m.root)
		}
		return m.buildStateLocked(), err
	}

	index := -1
	for i := range m.records {
		if m.records[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return rollback(i18n.NewError(i18n.KeyErrModNotFound))
	}
	activationID := m.records[index].ActivationID
	if included {
		for _, record := range m.records {
			if record.ActivationID == activationID && !record.Available {
				return rollback(i18n.NewError(i18n.KeyErrGroupUnreadable))
			}
		}
	}
	if m.settings.Enabled[m.root] == nil {
		m.settings.Enabled[m.root] = make(map[string]bool)
	}
	for i := range m.records {
		if m.records[i].ActivationID == activationID {
			m.records[i].Enabled = included
			m.settings.Enabled[m.root][m.records[i].ID] = included
		}
	}
	if err := m.persistLocked(); err != nil {
		return rollback(err)
	}
	return m.buildStateLocked(), nil
}

// SetAllIncluded selects every fully readable load entry, or clears all
// selections. Entries with an unreadable member are skipped when selecting.
func (m *Manager) SetAllIncluded(included bool) (AppState, int, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.records) == 0 {
		return m.buildStateLocked(), 0, nil
	}

	groupReadable := make(map[string]bool)
	for _, record := range m.records {
		readable, exists := groupReadable[record.ActivationID]
		if !exists {
			readable = true
		}
		groupReadable[record.ActivationID] = readable && record.Available
	}
	skippedGroups := 0
	if included {
		for _, readable := range groupReadable {
			if !readable {
				skippedGroups++
			}
		}
	}

	previousRecords := append([]mods.Mod(nil), m.records...)
	previousSelection, hadPreviousSelection := m.settings.Enabled[m.root]
	previousSelection = cloneBoolMap(previousSelection)
	if m.settings.Enabled[m.root] == nil {
		m.settings.Enabled[m.root] = make(map[string]bool)
	}
	for i := range m.records {
		selected := included && groupReadable[m.records[i].ActivationID]
		m.records[i].Enabled = selected
		m.settings.Enabled[m.root][m.records[i].ID] = selected
	}
	if err := m.persistLocked(); err != nil {
		m.records = previousRecords
		if hadPreviousSelection {
			m.settings.Enabled[m.root] = previousSelection
		} else {
			delete(m.settings.Enabled, m.root)
		}
		return m.buildStateLocked(), 0, err
	}
	return m.buildStateLocked(), skippedGroups, nil
}

// SetGameEnabled physically enables or disables one Starbound load entry.
func (m *Manager) SetGameEnabled(id string, enabled bool) (AppState, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	activationID := ""
	m.mu.RLock()
	for _, record := range m.records {
		if record.ID == id {
			activationID = record.ActivationID
			break
		}
	}
	m.mu.RUnlock()
	if activationID == "" {
		return m.GetState(), i18n.NewError(i18n.KeyErrModNotFound)
	}
	_, changed, err := m.setActivationGroup(activationID, enabled)
	if err != nil || !changed {
		return m.GetState(), err
	}
	return m.GetState(), nil
}

func recordIndexByPath(records []mods.Mod, path string) int {
	if path == "" {
		return -1
	}
	for i, record := range records {
		if filepath.Clean(record.Path) == filepath.Clean(path) {
			return i
		}
	}
	return -1
}

func (m *Manager) replaceGroupLocked(updated []mods.Mod) {
	byID := make(map[string]mods.Mod, len(updated))
	for _, record := range updated {
		byID[record.ID] = record
	}
	for i := range m.records {
		if record, ok := byID[m.records[i].ID]; ok {
			m.records[i] = record
		}
	}
}

func (m *Manager) applySavedGroupSelectionLocked() {
	saved := m.settings.Enabled[m.root]
	selected := make(map[string]bool)
	hasSaved := make(map[string]bool)
	for _, record := range m.records {
		if included, exists := saved[record.ID]; exists {
			hasSaved[record.ActivationID] = true
			if included {
				selected[record.ActivationID] = true
			}
		}
	}
	for i := range m.records {
		if hasSaved[m.records[i].ActivationID] {
			m.records[i].Enabled = selected[m.records[i].ActivationID] && m.records[i].Available
		}
	}
}

func (m *Manager) applySavedOrderLocked() {
	order := m.settings.Orders[m.root]
	rank := make(map[string]int, len(order))
	for i, id := range order {
		rank[id] = i
	}
	locked := make([]mods.Mod, 0, m.lockedCount)
	other := make([]mods.Mod, 0, len(m.records)-m.lockedCount)
	for _, record := range m.records {
		if record.OrderLocked {
			locked = append(locked, record)
		} else {
			other = append(other, record)
		}
	}
	if len(order) > 0 && len(other) > 1 {
		sort.SliceStable(other, func(i, j int) bool {
			rankI, existsI := rank[other[i].ID]
			rankJ, existsJ := rank[other[j].ID]
			if existsI && existsJ {
				return rankI < rankJ
			}
			return existsI
		})
	}
	m.records = append(locked, other...)
	m.lockedCount = len(locked)
}

func sortRecordsByPriority(records []mods.Mod) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].OrderLocked != records[j].OrderLocked {
			return records[i].OrderLocked
		}
		// Their relative order is irrelevant, and their existing priorities
		// are kept unchanged by the manager.
		if records[i].OrderLocked {
			return false
		}
		leftPriority, _ := metadataPriority(records[i].Source)
		rightPriority, _ := metadataPriority(records[j].Source)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		leftName, rightName := strings.ToLower(records[i].Name), strings.ToLower(records[j].Name)
		if leftName != rightName {
			return leftName < rightName
		}
		return strings.ToLower(records[i].Path) < strings.ToLower(records[j].Path)
	})
}

func readBaseAssetPriority(ctx context.Context, gameDir string) (float64, bool, error) {
	if gameDir == "" {
		return 0, false, nil
	}
	archivePath := filepath.Join(gameDir, "assets", "packed.pak")
	if _, err := os.Stat(archivePath); errors.Is(err, os.ErrNotExist) {
		return 0, true, nil
	} else if err != nil {
		return 0, false, i18n.NewError(i18n.KeyWarnBasePriorityRead, err)
	}
	archive, err := pak.Inspect(ctx, archivePath)
	if err != nil {
		return 0, false, i18n.NewError(i18n.KeyWarnBasePriorityRead, err)
	}
	priority, exists := metadataPriority(archive)
	if !exists {
		if _, specified := archive.Metadata["priority"]; specified {
			return 0, true, i18n.NewError(i18n.KeyWarnBasePriorityInvalid)
		}
		return 0, true, nil
	}
	return priority, true, nil
}

func metadataPriority(archive *pak.Archive) (float64, bool) {
	if archive == nil {
		return 0, false
	}
	value, exists := archive.Metadata["priority"]
	if !exists {
		return 0, false
	}
	var priority float64
	switch number := value.(type) {
	case float64:
		priority = number
	case float32:
		priority = float64(number)
	case int:
		priority = float64(number)
	case int8:
		priority = float64(number)
	case int16:
		priority = float64(number)
	case int32:
		priority = float64(number)
	case int64:
		priority = float64(number)
	case uint:
		priority = float64(number)
	case uint8:
		priority = float64(number)
	case uint16:
		priority = float64(number)
	case uint32:
		priority = float64(number)
	case uint64:
		priority = float64(number)
	default:
		return 0, false
	}
	if math.IsNaN(priority) || math.IsInf(priority, 0) {
		return 0, false
	}
	return priority, true
}

func orderedPriorityValues(records []mods.Mod) []float64 {
	priorities := make([]float64, len(records))
	nextPriority := 0
	for i, record := range records {
		if record.OrderLocked {
			priorities[i] = effectiveModPriority(record)
			continue
		}
		priorities[i] = float64(nextPriority)
		nextPriority++
	}
	return priorities
}

func priorityValuesMatchOrder(records []mods.Mod) bool {
	priorities := orderedPriorityValues(records)
	for i, record := range records {
		if record.OrderLocked {
			continue
		}
		if effectiveModPriority(record) != priorities[i] {
			return false
		}
	}
	return true
}

func writeOrderedPriorities(ctx context.Context, records []mods.Mod) ([]mods.Mod, error) {
	updated := append([]mods.Mod(nil), records...)
	priorities := orderedPriorityValues(updated)

	type change struct {
		index    int
		original *pak.Archive
	}
	var changes []change
	rollback := func(cause error) ([]mods.Mod, error) {
		var rollbackErrors []error
		for i := len(changes) - 1; i >= 0; i-- {
			changed := changes[i]
			current := updated[changed.index].Source
			restored, err := pak.RestoreMetadata(context.Background(), current, changed.original)
			if err != nil {
				rollbackErrors = append(rollbackErrors, i18n.WrapError(i18n.KeyErrPriorityRestore, err, updated[changed.index].Name))
				if inspected, inspectErr := pak.Inspect(context.Background(), changed.original.Path); inspectErr == nil {
					updated[changed.index].Source = inspected
				}
				continue
			}
			updated[changed.index].Source = restored
		}
		if len(rollbackErrors) > 0 {
			return updated, errors.Join(append([]error{cause}, rollbackErrors...)...)
		}
		return updated, cause
	}

	for i := range updated {
		if err := ctx.Err(); err != nil {
			return rollback(err)
		}
		mod := updated[i]
		if mod.OrderLocked {
			// Leave the source metadata untouched, including an absent priority.
			continue
		}
		if !mod.Available || mod.Source == nil {
			reason := mod.Error
			if reason == nil {
				reason = i18n.NewError(i18n.KeyErrModUnreadable)
			}
			return rollback(i18n.WrapError(i18n.KeyErrPriorityModUnreadable, reason, mod.Name))
		}
		currentPriority, hasPriority := metadataPriority(mod.Source)
		if hasPriority && currentPriority == priorities[i] {
			continue
		}
		metadata := cloneMetadata(mod.Source.Metadata)
		metadata["priority"] = priorities[i]
		original := mod.Source
		next, err := pak.WriteMetadata(ctx, original, metadata)
		if err != nil {
			return rollback(i18n.WrapError(i18n.KeyErrPriorityWrite, err, mod.Name))
		}
		updated[i].Source = next
		changes = append(changes, change{index: i, original: original})
	}
	return updated, nil
}

func cloneMetadata(metadata map[string]any) map[string]any {
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func (m *Manager) buildStateLocked() AppState {
	state := AppState{
		Language:               string(i18n.Normalize(m.settings.Language)),
		Root:                   m.root,
		GameDirectory:          m.gameDir,
		InstallDirectory:       gameModsDirectory(m.gameDir),
		MergedPackFileName:     storedMergedPackName(m.settings, m.gameDir),
		BaseAssetPriority:      m.baseAssetPriority,
		BaseAssetPriorityKnown: m.baseAssetPriorityKnown,
		Mods:                   make([]ModInfo, 0, len(m.records)),
		Warnings:               append([]error(nil), m.warnings...),
	}
	type assetOwner struct {
		name     string
		modIndex int
	}
	type assetSource struct {
		archive  *pak.Archive
		name     string
		modIndex int
	}
	normalOwners := make(map[string][]assetOwner)
	normalPaths := make(map[string]string)
	patchCounts := make(map[string]int)
	collisionsByMod := make([]int, len(m.records))
	assetPaths := make(map[string]struct{})
	var selectedSources []assetSource

	for _, record := range m.records {
		info := ModInfo{
			ID:             record.ID,
			Name:           record.Name,
			Version:        record.Version,
			Path:           record.Path,
			SourceType:     record.SourceType,
			SourceLocation: record.SourceLocation,
			OrderLocked:    record.OrderLocked,
			Available:      record.Available,
			Enabled:        record.Enabled,
			GameEnabled:    record.GameEnabled,
			Error:          record.Error,
		}
		if record.Source != nil {
			info.Priority, info.PriorityKnown = metadataPriority(record.Source)
			info.AssetCount = len(record.Source.Assets)
			for _, asset := range record.Source.Assets {
				if strings.HasSuffix(strings.ToLower(asset.Path), ".patch") {
					info.PatchCount++
				}
			}
		}
		if record.Enabled && record.Available && record.Source != nil {
			state.EnabledCount++
			selectedSources = append(selectedSources, assetSource{archive: record.Source, name: record.Name, modIndex: len(state.Mods)})
		}
		state.Mods = append(state.Mods, info)
	}
	for _, source := range selectedSources {
		if source.archive == nil {
			continue
		}
		seenNormalPaths := make(map[string]struct{})
		for _, asset := range source.archive.Assets {
			assetKey := strings.ToLower(asset.Path)
			assetPaths[assetKey] = struct{}{}
			if strings.HasSuffix(strings.ToLower(asset.Path), ".patch") {
				patchCounts[assetKey]++
				continue
			}
			if _, exists := seenNormalPaths[assetKey]; exists {
				continue
			}
			seenNormalPaths[assetKey] = struct{}{}
			normalOwners[assetKey] = append(normalOwners[assetKey], assetOwner{
				name: source.name, modIndex: source.modIndex,
			})
			if _, exists := normalPaths[assetKey]; !exists {
				normalPaths[assetKey] = asset.Path
			}
		}
	}

	paths := make([]string, 0, len(normalOwners))
	for assetPath, owners := range normalOwners {
		if len(owners) < 2 {
			continue
		}
		state.ConflictCount++
		paths = append(paths, assetPath)
		for _, owner := range owners {
			if owner.modIndex >= 0 {
				collisionsByMod[owner.modIndex]++
			}
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		left, right := strings.ToLower(paths[i]), strings.ToLower(paths[j])
		if left == right {
			return paths[i] < paths[j]
		}
		return left < right
	})
	for i, assetPath := range paths {
		if i >= 40 {
			break
		}
		owners := normalOwners[assetPath]
		ownerNames := make([]string, 0, len(owners))
		for _, owner := range owners {
			ownerNames = append(ownerNames, owner.name)
		}
		state.Conflicts = append(state.Conflicts, ConflictInfo{
			Path:   normalPaths[assetPath],
			Winner: owners[len(owners)-1].name,
			Mods:   ownerNames,
		})
	}
	for path, count := range patchCounts {
		state.PatchTargetCount++
		state.PatchFileCount += count
		assetPaths[path] = struct{}{}
	}
	state.AssetCount = len(assetPaths)
	state.CanMerge = state.EnabledCount > 0 && mods.IsGameRoot(state.GameDirectory)
	state.PriorityNeedsApply = !priorityValuesMatchOrder(m.records)
	for i := range state.Mods {
		state.Mods[i].ConflictCount = collisionsByMod[i]
		state.Mods[i].PriorityNumber = i + 1
	}
	return state
}

func storedMergedPackName(settings storedSettings, gameDir string) string {
	if name := strings.TrimSpace(settings.MergedPackNames[gameDir]); name != "" {
		if normalized, err := normalizedMergedPackFileName(name); err == nil {
			return normalized
		}
	}
	return defaultGeneratedPackName
}

func (m *Manager) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.filePath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.settings, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(m.filePath), ".settings-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if _, err := os.Lstat(m.filePath); errors.Is(err, os.ErrNotExist) {
		return os.Rename(tempPath, m.filePath)
	} else if err != nil {
		return err
	}
	backup, err := os.CreateTemp(filepath.Dir(m.filePath), ".settings-backup-*")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	if err := os.Rename(m.filePath, backupPath); err != nil {
		return err
	}
	if err := os.Rename(tempPath, m.filePath); err != nil {
		_ = os.Rename(backupPath, m.filePath)
		return err
	}
	_ = os.Remove(backupPath)
	return nil
}
