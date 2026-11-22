package pak

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"starbound-modmanager/internal/fswalk"
	"starbound-modmanager/internal/i18n"
)

const (
	archiveMagic = "SBAsset6"
	indexMagic   = "INDEX"
	maxEntries   = 500_000
	maxString    = 1 << 20
	maxJSONDepth = 128
)

// Asset points to one uncompressed file in a Starbound archive or directory.
type Asset struct {
	Path   string
	Offset uint64
	Size   uint64
}

// Archive describes either an unpacked mod directory or an SBAsset6 .pak file.
type Archive struct {
	Path         string
	Directory    bool
	FileSize     int64
	FileModTime  int64
	IndexOffset  uint64
	MetadataPath string
	Metadata     map[string]any
	Assets       []Asset
}

// Inspect lists the assets and metadata in a mod directory or .pak file.
func Inspect(ctx context.Context, sourcePath string) (*Archive, error) {
	absPath, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return inspectDirectory(ctx, absPath)
	}
	if !strings.EqualFold(filepath.Ext(absPath), ".pak") && !strings.HasSuffix(strings.ToLower(absPath), ".pak.bak") {
		return nil, i18n.NewError(i18n.KeyErrPakUnsupported, absPath)
	}
	return inspectPak(ctx, absPath)
}

// WriteMetadata replaces an archive's metadata while preserving its assets.
// For .pak files it appends a new index and then switches the index pointer,
// so the asset payload does not need to be copied or repacked.
func WriteMetadata(ctx context.Context, archive *Archive, metadata map[string]any) (*Archive, error) {
	if archive == nil {
		return nil, i18n.NewError(i18n.KeyErrPakMetadataModMissing)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metadata = cloneMetadata(metadata)
	if archive.Directory {
		return writeDirectoryMetadata(archive, metadata)
	}
	return writePakMetadata(ctx, archive, metadata)
}

// RestoreMetadata restores the previous metadata after a failed multi-mod
// priority update. A directory that did not originally contain metadata has
// its newly created metadata file removed.
func RestoreMetadata(ctx context.Context, current, original *Archive) (*Archive, error) {
	if current == nil || original == nil {
		return nil, i18n.NewError(i18n.KeyErrPakMetadataRestore)
	}
	if original.Directory && original.MetadataPath == "" && current.MetadataPath != "" {
		if err := os.Remove(current.MetadataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, i18n.WrapError(i18n.KeyErrPakMetadataRemove, err)
		}
		updated := *current
		updated.MetadataPath = ""
		updated.Metadata = cloneMetadata(original.Metadata)
		return &updated, nil
	}
	return WriteMetadata(ctx, current, original.Metadata)
}

func writeDirectoryMetadata(archive *Archive, metadata map[string]any) (*Archive, error) {
	metadataPath := archive.MetadataPath
	if metadataPath == "" {
		metadataPath = filepath.Join(archive.Path, "_metadata")
		if _, err := os.Lstat(metadataPath); err == nil {
			return nil, i18n.NewError(i18n.KeyErrPakMetadataNew)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else {
		currentData, err := os.ReadFile(metadataPath)
		if err != nil {
			return nil, err
		}
		var currentMetadata map[string]any
		if err := json.Unmarshal(currentData, &currentMetadata); err != nil {
			return nil, i18n.WrapError(i18n.KeyErrPakMetadataReadDir, err)
		}
		if currentMetadata == nil {
			currentMetadata = map[string]any{}
		}
		if !reflect.DeepEqual(currentMetadata, archive.Metadata) {
			return nil, i18n.NewError(i18n.KeyErrPakMetadataChanged)
		}
	}
	writePath := metadataPath
	if resolved, err := filepath.EvalSymlinks(metadataPath); err == nil {
		writePath = resolved
	} else if archive.MetadataPath != "" || !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(writePath); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, i18n.WrapError(i18n.KeyErrPakMetadataEncode, err)
	}
	temp, err := os.CreateTemp(filepath.Dir(writePath), ".smm-metadata-*.tmp")
	if err != nil {
		return nil, i18n.WrapError(i18n.KeyErrPakMetadataTemp, err)
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(mode); err != nil {
		return nil, err
	}
	if err := writeFull(temp, data); err != nil {
		return nil, err
	}
	if err := temp.Sync(); err != nil {
		return nil, err
	}
	if err := temp.Close(); err != nil {
		return nil, err
	}
	closed = true
	if err := replaceOutput(tempPath, writePath); err != nil {
		return nil, i18n.WrapError(i18n.KeyErrPakMetadataSave, err)
	}
	updated := *archive
	updated.MetadataPath = metadataPath
	updated.Metadata = cloneMetadata(metadata)
	return &updated, nil
}

func writePakMetadata(ctx context.Context, archive *Archive, metadata map[string]any) (*Archive, error) {
	indexData, err := encodeIndex(ctx, archive.Assets, metadata)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(archive.Path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if archive.FileSize > 0 && info.Size() != archive.FileSize {
		return nil, i18n.NewError(i18n.KeyErrPakChanged)
	}
	if archive.FileModTime != 0 && info.ModTime().UnixNano() != archive.FileModTime {
		return nil, i18n.NewError(i18n.KeyErrPakUpdated)
	}
	if info.Size() < 16 || uint64(info.Size()) > uint64(1<<63-1) {
		return nil, i18n.NewError(i18n.KeyErrPakInvalidSize)
	}
	var header [16]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return nil, err
	}
	if string(header[:8]) != archiveMagic {
		return nil, i18n.NewError(i18n.KeyErrPakHeader)
	}
	oldIndexOffset := binary.BigEndian.Uint64(header[8:16])
	if oldIndexOffset != archive.IndexOffset {
		return nil, i18n.NewError(i18n.KeyErrPakIndexChanged)
	}
	if uint64(len(indexData)) > uint64(1<<63-1)-uint64(info.Size()) {
		return nil, i18n.NewError(i18n.KeyErrPakIndexTooLarge)
	}
	newIndexOffset := uint64(info.Size())
	if err := writeAtFull(file, indexData, int64(newIndexOffset)); err != nil {
		_ = file.Truncate(info.Size())
		return nil, i18n.WrapError(i18n.KeyErrPakIndexWrite, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Truncate(info.Size())
		return nil, i18n.WrapError(i18n.KeyErrPakIndexSave, err)
	}
	if err := writeIndexPointer(file, newIndexOffset); err != nil {
		_ = writeIndexPointer(file, oldIndexOffset)
		_ = file.Sync()
		_ = file.Truncate(info.Size())
		return nil, i18n.WrapError(i18n.KeyErrPakIndexSwitch, err)
	}
	if err := file.Sync(); err != nil {
		_ = writeIndexPointer(file, oldIndexOffset)
		_ = file.Sync()
		_ = file.Truncate(info.Size())
		return nil, i18n.WrapError(i18n.KeyErrPakPointerSave, err)
	}

	activeIndexOffset := newIndexOffset
	newFileSize := info.Size() + int64(len(indexData))
	oldIndexEnd := oldIndexOffset + uint64(len(indexData))
	if oldIndexOffset <= newIndexOffset && oldIndexEnd >= oldIndexOffset && oldIndexEnd <= newIndexOffset {
		if err := writeAtFull(file, indexData, int64(oldIndexOffset)); err == nil && file.Sync() == nil {
			if err := writeIndexPointer(file, oldIndexOffset); err == nil && file.Sync() == nil {
				activeIndexOffset = oldIndexOffset
				if err := file.Truncate(int64(oldIndexEnd)); err == nil {
					newFileSize = int64(oldIndexEnd)
					_ = file.Sync()
				}
			} else {
				_ = writeIndexPointer(file, newIndexOffset)
				_ = file.Sync()
			}
		}
	}
	var activePointer [8]byte
	if _, err := file.ReadAt(activePointer[:], 8); err == nil {
		pointer := binary.BigEndian.Uint64(activePointer[:])
		if pointer == oldIndexOffset || pointer == newIndexOffset {
			activeIndexOffset = pointer
		}
	}
	if finalInfo, err := file.Stat(); err == nil {
		newFileSize = finalInfo.Size()
	}
	updated := *archive
	updated.FileSize = newFileSize
	updated.IndexOffset = activeIndexOffset
	updated.Metadata = cloneMetadata(metadata)
	if finalInfo, err := file.Stat(); err == nil {
		updated.FileModTime = finalInfo.ModTime().UnixNano()
	}
	return &updated, nil
}

func encodeIndex(ctx context.Context, assets []Asset, metadata map[string]any) ([]byte, error) {
	if len(assets) > maxEntries {
		return nil, i18n.NewError(i18n.KeyErrPakEntryLimit, len(assets))
	}
	buffer := &bytes.Buffer{}
	if err := writeFull(buffer, []byte(indexMagic)); err != nil {
		return nil, err
	}
	if err := writeJSONObject(buffer, metadata); err != nil {
		return nil, i18n.WrapError(i18n.KeyErrPakIndexMetadataWrite, err)
	}
	if err := writeVLQU(buffer, uint64(len(assets))); err != nil {
		return nil, err
	}
	for _, asset := range assets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := writeString(buffer, asset.Path); err != nil {
			return nil, err
		}
		var pair [16]byte
		binary.BigEndian.PutUint64(pair[:8], asset.Offset)
		binary.BigEndian.PutUint64(pair[8:], asset.Size)
		if err := writeFull(buffer, pair[:]); err != nil {
			return nil, err
		}
	}
	return buffer.Bytes(), nil
}

func writeAtFull(file *os.File, data []byte, offset int64) error {
	for len(data) > 0 {
		n, err := file.WriteAt(data, offset)
		if n > 0 {
			data = data[n:]
			offset += int64(n)
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func writeIndexPointer(file *os.File, offset uint64) error {
	var pointer [8]byte
	binary.BigEndian.PutUint64(pointer[:], offset)
	return writeAtFull(file, pointer[:], 8)
}

func cloneMetadata(metadata map[string]any) map[string]any {
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func inspectDirectory(ctx context.Context, root string) (*Archive, error) {
	archive := &Archive{
		Path:      root,
		Directory: true,
		Metadata:  map[string]any{},
		Assets:    make([]Asset, 0),
	}
	for _, name := range []string{"_metadata", ".metadata"} {
		metadataPath := filepath.Join(root, name)
		data, err := os.ReadFile(metadataPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, i18n.WrapError(i18n.KeyErrMetadataReadFile, err, metadataPath)
		}
		if err := json.Unmarshal(data, &archive.Metadata); err != nil {
			return nil, i18n.WrapError(i18n.KeyErrMetadataParseFile, err, metadataPath)
		}
		if archive.Metadata == nil {
			archive.Metadata = map[string]any{}
		}
		archive.MetadataPath = metadataPath
		break
	}

	err := fswalk.Files(ctx, root, func(filePath, relative string, info os.FileInfo) error {
		if relative == "_metadata" || relative == ".metadata" {
			return nil
		}
		assetPath := "/" + filepath.ToSlash(relative)
		if err := validateAssetPath(assetPath); err != nil {
			return i18n.WrapError(i18n.KeyErrDirectoryAssetPath, err, root)
		}
		archive.Assets = append(archive.Assets, Asset{Path: assetPath, Size: uint64(info.Size())})
		return nil
	})
	if err != nil {
		return nil, i18n.WrapError(i18n.KeyErrScanModDirectory, err, root)
	}
	return archive, nil
}

func inspectPak(ctx context.Context, archivePath string) (*Archive, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 16 {
		return nil, i18n.NewError(i18n.KeyErrPakNotAsset6)
	}

	var header [16]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return nil, err
	}
	if string(header[:8]) != archiveMagic {
		return nil, i18n.NewError(i18n.KeyErrPakHeader)
	}
	indexStart := binary.BigEndian.Uint64(header[8:16])
	if indexStart < 16 || indexStart > uint64(info.Size()-int64(len(indexMagic))) {
		return nil, i18n.NewError(i18n.KeyErrPakIndexBounds)
	}
	section := io.NewSectionReader(file, int64(indexStart), info.Size()-int64(indexStart))
	reader := bufio.NewReader(section)

	var indexHeader [len(indexMagic)]byte
	if _, err := io.ReadFull(reader, indexHeader[:]); err != nil {
		return nil, i18n.WrapError(i18n.KeyErrPakIndexHeaderRead, err)
	}
	if string(indexHeader[:]) != indexMagic {
		return nil, i18n.NewError(i18n.KeyErrPakIndexMissing)
	}

	metadata, err := readJSONObject(reader, 0)
	if err != nil {
		return nil, i18n.WrapError(i18n.KeyErrPakMetadataRead, err)
	}
	count, err := readVLQU(reader)
	if err != nil {
		return nil, i18n.WrapError(i18n.KeyErrPakCountRead, err)
	}
	if count > maxEntries {
		return nil, i18n.NewError(i18n.KeyErrPakCountLimit, count)
	}

	assets := make([]Asset, 0, int(count))
	seen := make(map[string]struct{}, int(count))
	for i := uint64(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		assetPath, err := readString(reader)
		if err != nil {
			return nil, i18n.WrapError(i18n.KeyErrPakAssetPathRead, err, i+1)
		}
		if err := validateAssetPath(assetPath); err != nil {
			return nil, i18n.WrapError(i18n.KeyErrPakAssetPathInvalid, err)
		}
		var pair [16]byte
		if _, err := io.ReadFull(reader, pair[:]); err != nil {
			return nil, i18n.WrapError(i18n.KeyErrPakAssetOffsetRead, err, assetPath)
		}
		offset := binary.BigEndian.Uint64(pair[:8])
		size := binary.BigEndian.Uint64(pair[8:])
		if offset < 16 || offset > indexStart || size > indexStart-offset {
			return nil, i18n.NewError(i18n.KeyErrPakAssetRange, assetPath)
		}
		if offset > 1<<63-1 || size > 1<<63-1 {
			return nil, i18n.NewError(i18n.KeyErrPakAssetSize, assetPath)
		}
		if _, exists := seen[assetPath]; exists {
			return nil, i18n.NewError(i18n.KeyErrPakAssetDuplicate, assetPath)
		}
		seen[assetPath] = struct{}{}
		assets = append(assets, Asset{Path: assetPath, Offset: offset, Size: size})
	}

	return &Archive{
		Path:        archivePath,
		Directory:   false,
		FileSize:    info.Size(),
		FileModTime: info.ModTime().UnixNano(),
		IndexOffset: indexStart,
		Metadata:    metadata,
		Assets:      assets,
	}, nil
}

func validateAssetPath(assetPath string) error {
	if !strings.HasPrefix(assetPath, "/") || assetPath == "/" {
		return i18n.NewError(i18n.KeyErrPakPathAbsolute, assetPath)
	}
	if strings.ContainsAny(assetPath, "\\\x00") || path.Clean(assetPath) != assetPath {
		return i18n.NewError(i18n.KeyErrPakPathSegment, assetPath)
	}
	for _, part := range strings.Split(strings.TrimPrefix(assetPath, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return i18n.NewError(i18n.KeyErrPakPathSegment, assetPath)
		}
	}
	return nil
}

func readJSONObject(reader *bufio.Reader, depth int) (map[string]any, error) {
	if depth > maxJSONDepth {
		return nil, i18n.NewError(i18n.KeyErrPakDepth)
	}
	count, err := readVLQU(reader)
	if err != nil {
		return nil, err
	}
	if count > maxEntries {
		return nil, i18n.NewError(i18n.KeyErrPakFieldLimit, count)
	}
	object := make(map[string]any, int(count))
	for i := uint64(0); i < count; i++ {
		key, err := readString(reader)
		if err != nil {
			return nil, err
		}
		value, err := readJSONValue(reader, depth+1)
		if err != nil {
			return nil, i18n.WrapError(i18n.KeyErrPakMetadataFieldRead, err, key)
		}
		object[key] = value
	}
	return object, nil
}

func readJSONValue(reader *bufio.Reader, depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, i18n.NewError(i18n.KeyErrPakJSONDepth)
	}
	tag, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	switch tag {
	case 0, 1: // Old streams could serialize the invalid value as zero.
		return nil, nil
	case 2:
		var raw [8]byte
		if _, err := io.ReadFull(reader, raw[:]); err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(raw[:])), nil
	case 3:
		value, err := reader.ReadByte()
		return value != 0, err
	case 4:
		return readVLQI(reader)
	case 5:
		return readString(reader)
	case 6:
		count, err := readVLQU(reader)
		if err != nil {
			return nil, err
		}
		if count > maxEntries {
			return nil, i18n.NewError(i18n.KeyErrPakArrayLimit, count)
		}
		values := make([]any, 0, int(count))
		for i := uint64(0); i < count; i++ {
			value, err := readJSONValue(reader, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	case 7:
		return readJSONObject(reader, depth+1)
	default:
		return nil, i18n.NewError(i18n.KeyErrPakJSONTag, tag)
	}
}

func readString(reader *bufio.Reader) (string, error) {
	length, err := readVLQU(reader)
	if err != nil {
		return "", err
	}
	if length > maxString {
		return "", i18n.NewError(i18n.KeyErrPakStringLimit, length)
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(reader, data); err != nil {
		return "", err
	}
	return string(data), nil
}

func readVLQU(reader *bufio.Reader) (uint64, error) {
	var value uint64
	for i := 0; i < 10; i++ {
		b, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		if value > ^uint64(0)>>7 {
			return 0, i18n.NewError(i18n.KeyErrPakVLQOverflow)
		}
		value = value<<7 | uint64(b&0x7f)
		if b&0x80 == 0 {
			return value, nil
		}
	}
	return 0, i18n.NewError(i18n.KeyErrPakVLQLength)
}

func readVLQI(reader *bufio.Reader) (int64, error) {
	encoded, err := readVLQU(reader)
	if err != nil {
		return 0, err
	}
	if encoded&1 != 0 {
		return -int64(encoded>>1) - 1, nil
	}
	return int64(encoded >> 1), nil
}
