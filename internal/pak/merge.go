package pak

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"starbound-modmanager/internal/i18n"
)

const maxPatchSize = 64 << 20

var copyBufferPool = sync.Pool{New: func() any { return make([]byte, 128<<10) }}

// Progress describes one file copied while a merged archive is being built.
type Progress struct {
	Current int
	Total   int
	Path    string
}

// Report summarizes a successfully written archive.
type Report struct {
	OutputPath    string
	AssetCount    int
	ConflictCount int
	PatchTargets  int
	PatchFiles    int
	SizeBytes     int64
}

type assetRef struct {
	archive *Archive
	asset   Asset
}

type outputAsset struct {
	path string
	ref  assetRef
	data []byte
}

type indexEntry struct {
	path   string
	offset uint64
	size   uint64
}

// Merge writes an SBAsset6 archive. Sources are ordered from highest to lowest
// priority; later Starbound sources override earlier ones, so the first source
// wins ordinary file collisions. Patch files are combined in reverse order so
// their operations still run from lowest to highest priority.
func Merge(ctx context.Context, sources []*Archive, outputPath string, metadata map[string]any, onProgress func(Progress)) (Report, error) {
	if len(sources) == 0 {
		return Report{}, i18n.NewError(i18n.KeyErrMergeNoSources)
	}
	outAbs, err := filepath.Abs(outputPath)
	if err != nil {
		return Report{}, err
	}
	if info, statErr := os.Lstat(outAbs); statErr == nil && info.IsDir() {
		return Report{}, i18n.NewError(i18n.KeyErrMergeOutputFolder)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return Report{}, statErr
	}
	if err := ensureOutputOutsideSources(sources, outAbs); err != nil {
		return Report{}, err
	}
	if metadata == nil {
		metadata = map[string]any{}
	}

	winners := make(map[string]assetRef)
	owners := make(map[string]int)
	patches := make(map[string][]assetRef)
	patchPaths := make(map[string]string)
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		if source == nil {
			continue
		}
		sourceAssets := make(map[string]Asset)
		for _, asset := range source.Assets {
			key := strings.ToLower(asset.Path)
			if isPatchPath(asset.Path) {
				patches[key] = append(patches[key], assetRef{archive: source, asset: asset})
				if _, exists := patchPaths[key]; !exists {
					patchPaths[key] = asset.Path
				}
				continue
			}
			// Starbound indexes resource paths case-insensitively. If a single
			// mod contains case-only duplicates, keep the last indexed version.
			sourceAssets[key] = asset
		}
		for key, asset := range sourceAssets {
			owners[key]++
			if _, exists := winners[key]; !exists {
				winners[key] = assetRef{archive: source, asset: asset}
			}
		}
	}

	assets := make([]outputAsset, 0, len(winners)+len(patches))
	for _, ref := range winners {
		assets = append(assets, outputAsset{path: ref.asset.Path, ref: ref})
	}
	report := Report{OutputPath: outAbs}
	for _, count := range owners {
		if count > 1 {
			report.ConflictCount++
		}
	}
	for key, refs := range patches {
		report.PatchTargets++
		report.PatchFiles += len(refs)
		if len(refs) == 1 {
			assets = append(assets, outputAsset{path: patchPaths[key], ref: refs[0]})
			continue
		}
		data, err := combinePatchFiles(ctx, refs)
		if err != nil {
			return Report{}, i18n.WrapError(i18n.KeyErrMergePatch, err, patchPaths[key])
		}
		assets = append(assets, outputAsset{path: patchPaths[key], data: data})
	}
	sort.Slice(assets, func(i, j int) bool {
		left, right := strings.ToLower(assets[i].path), strings.ToLower(assets[j].path)
		if left == right {
			return assets[i].path < assets[j].path
		}
		return left < right
	})

	outputDir := filepath.Dir(outAbs)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return Report{}, i18n.WrapError(i18n.KeyErrMergeCreateOutput, err)
	}
	baseName := filepath.Base(outAbs)
	temp, err := os.CreateTemp(outputDir, "."+baseName+"-*.tmp")
	if err != nil {
		return Report{}, i18n.WrapError(i18n.KeyErrMergeCreateTemp, err)
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempPath)
	}()

	if err := writeFull(temp, []byte(archiveMagic)); err != nil {
		return Report{}, err
	}
	if err := writeFull(temp, make([]byte, 8)); err != nil {
		return Report{}, err
	}
	index := make([]indexEntry, 0, len(assets))
	for i, asset := range assets {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		if onProgress != nil {
			onProgress(Progress{Current: i + 1, Total: len(assets), Path: asset.path})
		}
		position, err := temp.Seek(0, io.SeekCurrent)
		if err != nil {
			return Report{}, err
		}
		var size uint64
		if asset.data != nil {
			if err := writeFull(temp, asset.data); err != nil {
				return Report{}, i18n.WrapError(i18n.KeyErrMergeWriteAsset, err, asset.path)
			}
			size = uint64(len(asset.data))
		} else {
			written, err := copyAsset(ctx, temp, asset.ref)
			if err != nil {
				return Report{}, i18n.WrapError(i18n.KeyErrMergeCopyAsset, err, asset.path)
			}
			size = uint64(written)
		}
		index = append(index, indexEntry{path: asset.path, offset: uint64(position), size: size})
	}

	indexStart, err := temp.Seek(0, io.SeekCurrent)
	if err != nil {
		return Report{}, err
	}
	if err := writeFull(temp, []byte(indexMagic)); err != nil {
		return Report{}, err
	}
	if err := writeJSONObject(temp, metadata); err != nil {
		return Report{}, i18n.WrapError(i18n.KeyErrPakIndexMetadataWrite, err)
	}
	if err := writeVLQU(temp, uint64(len(index))); err != nil {
		return Report{}, err
	}
	for _, entry := range index {
		if err := writeString(temp, entry.path); err != nil {
			return Report{}, err
		}
		var pair [16]byte
		binary.BigEndian.PutUint64(pair[:8], entry.offset)
		binary.BigEndian.PutUint64(pair[8:], entry.size)
		if err := writeFull(temp, pair[:]); err != nil {
			return Report{}, err
		}
	}
	if _, err := temp.Seek(8, io.SeekStart); err != nil {
		return Report{}, err
	}
	var indexPointer [8]byte
	binary.BigEndian.PutUint64(indexPointer[:], uint64(indexStart))
	if err := writeFull(temp, indexPointer[:]); err != nil {
		return Report{}, err
	}
	if err := temp.Sync(); err != nil {
		return Report{}, err
	}
	if err := temp.Close(); err != nil {
		return Report{}, err
	}
	closed = true
	if err := replaceOutput(tempPath, outAbs); err != nil {
		return Report{}, i18n.WrapError(i18n.KeyErrMergeSave, err)
	}
	info, err := os.Stat(outAbs)
	if err != nil {
		return Report{}, err
	}
	report.AssetCount = len(index)
	report.SizeBytes = info.Size()
	return report, nil
}

func ensureOutputOutsideSources(sources []*Archive, output string) error {
	for _, source := range sources {
		if source == nil {
			continue
		}
		sourcePath, err := filepath.Abs(source.Path)
		if err != nil {
			return err
		}
		if samePath(sourcePath, output) {
			return i18n.NewError(i18n.KeyErrMergeOutputSource)
		}
		if !source.Directory {
			continue
		}
		relative, err := filepath.Rel(sourcePath, output)
		if err != nil {
			continue
		}
		if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return i18n.NewError(i18n.KeyErrMergeOutputInside)
		}
	}
	return nil
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func isPatchPath(assetPath string) bool {
	return strings.HasSuffix(strings.ToLower(assetPath), ".patch")
}

func copyAsset(ctx context.Context, dst io.Writer, ref assetRef) (int64, error) {
	reader, err := openAsset(ref)
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	return copyExactly(ctx, dst, reader, ref.asset.Size)
}

type readCloser struct {
	io.Reader
	io.Closer
}

func openAsset(ref assetRef) (io.ReadCloser, error) {
	if ref.archive.Directory {
		relative := strings.TrimPrefix(ref.asset.Path, "/")
		clean := filepath.Clean(filepath.FromSlash(relative))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, i18n.NewError(i18n.KeyErrMergeAssetPath, ref.asset.Path)
		}
		fullPath := filepath.Join(ref.archive.Path, clean)
		file, err := os.Open(fullPath)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, err
		}
		if uint64(info.Size()) != ref.asset.Size {
			file.Close()
			return nil, i18n.NewError(i18n.KeyErrMergeAssetChanged, ref.asset.Path)
		}
		return file, nil
	}

	if ref.asset.Offset > 1<<63-1 || ref.asset.Size > 1<<63-1 {
		return nil, i18n.NewError(i18n.KeyErrMergeAssetTooLarge, ref.asset.Path)
	}
	file, err := os.Open(ref.archive.Path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if ref.archive.FileSize > 0 && info.Size() != ref.archive.FileSize {
		file.Close()
		return nil, i18n.NewError(i18n.KeyErrMergePakChanged)
	}
	return readCloser{Reader: io.NewSectionReader(file, int64(ref.asset.Offset), int64(ref.asset.Size)), Closer: file}, nil
}

func copyExactly(ctx context.Context, dst io.Writer, src io.Reader, size uint64) (int64, error) {
	buffer := copyBufferPool.Get().([]byte)
	defer copyBufferPool.Put(buffer)
	var copied uint64
	for copied < size {
		if err := ctx.Err(); err != nil {
			return int64(copied), err
		}
		chunk := uint64(len(buffer))
		if remaining := size - copied; remaining < chunk {
			chunk = remaining
		}
		n, err := io.ReadFull(src, buffer[:int(chunk)])
		if n > 0 {
			if writeErr := writeFull(dst, buffer[:n]); writeErr != nil {
				return int64(copied), writeErr
			}
			copied += uint64(n)
		}
		if err != nil {
			return int64(copied), err
		}
	}
	return int64(copied), nil
}

func combinePatchFiles(ctx context.Context, highToLow []assetRef) ([]byte, error) {
	groups := make([]json.RawMessage, 0)
	for end := len(highToLow); end > 0; {
		start := end - 1
		source := highToLow[start].archive
		for start > 0 && highToLow[start-1].archive == source {
			start--
		}
		// Apply source groups from low to high priority, keeping the order of
		// patch files within each source as Starbound reports them.
		for i := start; i < end; i++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ref := highToLow[i]
			if ref.asset.Size > maxPatchSize {
				return nil, i18n.NewError(i18n.KeyErrMergePatchTooLarge, maxPatchSize>>20)
			}
			reader, err := openAsset(ref)
			if err != nil {
				return nil, err
			}
			data, readErr := io.ReadAll(io.LimitReader(reader, int64(ref.asset.Size)+1))
			closeErr := reader.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if uint64(len(data)) != ref.asset.Size {
				return nil, i18n.NewError(i18n.KeyErrMergePatchChanged, ref.asset.Path)
			}

			trimmed := strings.TrimSpace(string(data))
			if !strings.HasPrefix(trimmed, "[") {
				return nil, i18n.NewError(i18n.KeyErrMergePatchArray)
			}
			var items []json.RawMessage
			if err := json.Unmarshal(data, &items); err != nil {
				return nil, err
			}
			if len(items) == 0 {
				continue
			}
			first := strings.TrimSpace(string(items[0]))
			switch {
			case strings.HasPrefix(first, "["):
				for _, item := range items {
					if !jsonHasType(item, '[') {
						return nil, i18n.NewError(i18n.KeyErrMergePatchShape)
					}
					var operations []json.RawMessage
					if err := json.Unmarshal(item, &operations); err != nil {
						return nil, err
					}
					if err := validatePatchOperations(operations); err != nil {
						return nil, err
					}
					groups = append(groups, item)
				}
			case strings.HasPrefix(first, "{"):
				if err := validatePatchOperations(items); err != nil {
					return nil, err
				}
				groups = append(groups, json.RawMessage(data))
			default:
				return nil, i18n.NewError(i18n.KeyErrMergePatchObject)
			}
		}
		end = start
	}
	return json.Marshal(groups)
}

func jsonHasType(raw json.RawMessage, want byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	return len(trimmed) > 0 && trimmed[0] == want
}

func validatePatchOperations(operations []json.RawMessage) error {
	for _, operation := range operations {
		if !jsonHasType(operation, '{') {
			return i18n.NewError(i18n.KeyErrMergePatchObject)
		}
	}
	return nil
}

func replaceOutput(tempPath, outputPath string) error {
	if _, err := os.Lstat(outputPath); errors.Is(err, os.ErrNotExist) {
		return os.Rename(tempPath, outputPath)
	} else if err != nil {
		return err
	}

	directory := filepath.Dir(outputPath)
	backup, err := os.CreateTemp(directory, "."+filepath.Base(outputPath)+"-backup-*")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		os.Remove(backupPath)
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	if err := os.Rename(outputPath, backupPath); err != nil {
		return err
	}
	if err := os.Rename(tempPath, outputPath); err != nil {
		_ = os.Rename(backupPath, outputPath)
		return err
	}
	_ = os.Remove(backupPath)
	return nil
}

func writeJSONObject(writer io.Writer, object map[string]any) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := writeVLQU(writer, uint64(len(keys))); err != nil {
		return err
	}
	for _, key := range keys {
		if err := writeString(writer, key); err != nil {
			return err
		}
		if err := writeJSONValue(writer, object[key], 0); err != nil {
			return i18n.WrapError(i18n.KeyErrPakMetadataFieldWrite, err, key)
		}
	}
	return nil
}

func writeJSONValue(writer io.Writer, value any, depth int) error {
	if depth > maxJSONDepth {
		return i18n.NewError(i18n.KeyErrMergeJSONDepth)
	}
	writeTag := func(tag byte) error { return writeFull(writer, []byte{tag}) }
	switch v := value.(type) {
	case nil:
		return writeTag(1)
	case bool:
		if err := writeTag(3); err != nil {
			return err
		}
		if v {
			return writeFull(writer, []byte{1})
		}
		return writeFull(writer, []byte{0})
	case string:
		if err := writeTag(5); err != nil {
			return err
		}
		return writeString(writer, v)
	case int:
		if err := writeTag(4); err != nil {
			return err
		}
		return writeVLQI(writer, int64(v))
	case int8:
		if err := writeTag(4); err != nil {
			return err
		}
		return writeVLQI(writer, int64(v))
	case int16:
		if err := writeTag(4); err != nil {
			return err
		}
		return writeVLQI(writer, int64(v))
	case int32:
		if err := writeTag(4); err != nil {
			return err
		}
		return writeVLQI(writer, int64(v))
	case int64:
		if err := writeTag(4); err != nil {
			return err
		}
		return writeVLQI(writer, v)
	case float32:
		return writeFloat(writer, float64(v), writeTag)
	case float64:
		return writeFloat(writer, v, writeTag)
	case []any:
		if err := writeTag(6); err != nil {
			return err
		}
		if err := writeVLQU(writer, uint64(len(v))); err != nil {
			return err
		}
		for _, item := range v {
			if err := writeJSONValue(writer, item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if err := writeTag(7); err != nil {
			return err
		}
		return writeJSONObject(writer, v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		var normalized any
		if err := json.Unmarshal(encoded, &normalized); err != nil {
			return err
		}
		return writeJSONValue(writer, normalized, depth+1)
	}
}

func writeFloat(writer io.Writer, value float64, writeTag func(byte) error) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return i18n.NewError(i18n.KeyErrMergeNonFinite)
	}
	if err := writeTag(2); err != nil {
		return err
	}
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], math.Float64bits(value))
	return writeFull(writer, raw[:])
}

func writeString(writer io.Writer, value string) error {
	if err := writeVLQU(writer, uint64(len(value))); err != nil {
		return err
	}
	return writeFull(writer, []byte(value))
}

func writeVLQU(writer io.Writer, value uint64) error {
	var groups [10]byte
	count := 0
	for {
		groups[count] = byte(value & 0x7f)
		count++
		value >>= 7
		if value == 0 {
			break
		}
	}
	for i := count - 1; i >= 0; i-- {
		b := groups[i]
		if i != 0 {
			b |= 0x80
		}
		if err := writeFull(writer, []byte{b}); err != nil {
			return err
		}
	}
	return nil
}

func writeVLQI(writer io.Writer, value int64) error {
	var encoded uint64
	if value < 0 {
		encoded = uint64(-(value+1))<<1 | 1
	} else {
		encoded = uint64(value) << 1
	}
	return writeVLQU(writer, encoded)
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
