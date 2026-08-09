// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/inventory"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/memfd"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/mm"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pagemap"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/vma"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

const ManifestFilename = "pagebroker-manifest.yaml"
const wireManifestFilename = "pagebroker-manifest.pb"

const (
	vmaAnonShared  = 1 << 8
	vmaAnonPrivate = 1 << 9
	mapGrowsDown   = 0x100
	madvWipeOnFork = 18
	peParent       = 1 << 0
	peLazy         = 1 << 1
	pePresent      = 1 << 2
	pePayloadAlign = 1 << 3
)

var errUnsupportedManifest = errors.New("unsupported PageBroker checkpoint format")

// IsUnsupported reports whether a checkpoint must use the legacy restore path.
func IsUnsupported(err error) bool { return errors.Is(err, errUnsupportedManifest) }

type Image struct {
	URI  string `yaml:"uri"`
	Size uint64 `yaml:"size"`
}

type SourceRange struct {
	Object       string `yaml:"object"`
	SourceOffset uint64 `yaml:"source_offset"`
	DstOffset    uint64 `yaml:"dst_offset"`
	Length       uint64 `yaml:"length"`
}

type HostMemoryObject struct {
	MemoryID    uint64        `yaml:"memory_id"`
	Name        string        `yaml:"name"`
	PID         uint32        `yaml:"pid,omitempty"`
	VMAID       uint32        `yaml:"vma_id,omitempty"`
	Shmid       uint64        `yaml:"shmid,omitempty"`
	DstAddr     uint64        `yaml:"dst_addr,omitempty"`
	Length      uint64        `yaml:"length"`
	Semantics   string        `yaml:"mem_semantics"`
	MapMode     string        `yaml:"map_mode"`
	SourceRange []SourceRange `yaml:"source_ranges,omitempty"`
}

type Manifest struct {
	Version           uint32             `yaml:"version"`
	CheckpointID      string             `yaml:"checkpoint_id"`
	ResidentBytes     uint64             `yaml:"resident_bytes"`
	Images            map[string]Image   `yaml:"images"`
	HostMemoryObjects []HostMemoryObject `yaml:"host_memory_objects"`
}

func ReadPageBrokerManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode PageBroker manifest: %w", err)
	}
	if err := manifest.validate(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func (m *Manifest) validate(root string) error {
	if m.Version != 1 {
		return fmt.Errorf("%w: manifest version %d", errUnsupportedManifest, m.Version)
	}
	var resident uint64
	for _, object := range m.HostMemoryObjects {
		if object.Length == 0 {
			return fmt.Errorf("invalid PageBroker object %q: zero length", object.Name)
		}
		for _, source := range object.SourceRange {
			image, ok := m.Images[source.Object]
			if !ok {
				return fmt.Errorf("PageBroker object %q references unknown image %q", object.Name, source.Object)
			}
			if source.Length == 0 || source.SourceOffset > image.Size || source.Length > image.Size-source.SourceOffset || source.DstOffset > object.Length || source.Length > object.Length-source.DstOffset {
				return fmt.Errorf("invalid source range for PageBroker object %q", object.Name)
			}
			if resident > ^uint64(0)-source.Length {
				return fmt.Errorf("PageBroker resident-byte requirement overflows")
			}
			resident += source.Length
			cleanURI := filepath.Clean(image.URI)
			if filepath.IsAbs(image.URI) || cleanURI == ".." || strings.HasPrefix(cleanURI, ".."+string(filepath.Separator)) {
				return fmt.Errorf("invalid PageBroker image URI %q", image.URI)
			}
			if _, err := os.Stat(filepath.Join(root, image.URI)); err != nil {
				return fmt.Errorf("open PageBroker source image %q: %w", source.Object, err)
			}
		}
	}
	if resident != m.ResidentBytes {
		return fmt.Errorf("PageBroker resident bytes %d do not match object total %d", m.ResidentBytes, resident)
	}
	return nil
}

// EnsureManifest generates the sidecar for checkpoints created before the
// PageBroker packaging path existed.
func EnsureManifest(checkpointDir string) (string, *Manifest, error) {
	path := filepath.Join(checkpointDir, ManifestFilename)
	manifest, err := ReadPageBrokerManifest(path)
	if err == nil {
		if err := writeWireManifest(checkpointDir, manifest); err != nil {
			return "", nil, err
		}
		return path, manifest, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	checkpoint, err := types.ReadManifest(checkpointDir)
	if err != nil {
		return "", nil, err
	}
	manifest, err = GenerateManifest(checkpointDir, checkpoint.CheckpointID)
	if err != nil {
		return "", nil, err
	}
	return path, manifest, nil
}

func GenerateManifest(checkpointDir, checkpointID string) (*Manifest, error) {
	manifest := &Manifest{
		Version:      1,
		CheckpointID: checkpointID,
		Images:       make(map[string]Image),
	}
	if err := rejectCompressedInventory(checkpointDir); err != nil {
		return nil, err
	}
	if err := addImages(checkpointDir, manifest); err != nil {
		return nil, err
	}
	if err := addMemoryObjects(checkpointDir, manifest); err != nil {
		return nil, err
	}
	for index := range manifest.HostMemoryObjects {
		object := &manifest.HostMemoryObjects[index]
		for _, source := range object.SourceRange {
			if manifest.ResidentBytes > ^uint64(0)-source.Length {
				return nil, fmt.Errorf("PageBroker resident-byte requirement overflows")
			}
			manifest.ResidentBytes += source.Length
		}
	}
	data, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(checkpointDir, ManifestFilename)
	if err := writeAtomic(path, data); err != nil {
		return nil, fmt.Errorf("write %s: %w", ManifestFilename, err)
	}
	if err := writeWireManifest(checkpointDir, manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func writeWireManifest(checkpointDir string, manifest *Manifest) error {
	path := filepath.Join(checkpointDir, wireManifestFilename)
	if err := writeAtomic(path, manifestWireData(manifest)); err != nil {
		return fmt.Errorf("write %s: %w", wireManifestFilename, err)
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func decodeImage(path string, entry proto.Message) (*crit.CriuImage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	image, err := crit.New(file, nil, "", false, true).Decode(entry)
	if err != nil {
		return nil, fmt.Errorf("decode CRIU image %s: %w", filepath.Base(path), err)
	}
	return image, nil
}

func rejectCompressedInventory(checkpointDir string) error {
	image, err := decodeImage(filepath.Join(checkpointDir, "inventory.img"), &inventory.InventoryEntry{})
	if err != nil {
		return err
	}
	if len(image.Entries) != 1 {
		return fmt.Errorf("invalid inventory image: got %d entries", len(image.Entries))
	}
	unknown := image.Entries[0].Message.ProtoReflect().GetUnknown()
	for len(unknown) > 0 {
		number, wire, tagLength := protowire.ConsumeTag(unknown)
		if tagLength < 0 {
			return fmt.Errorf("invalid inventory protobuf extensions")
		}
		unknown = unknown[tagLength:]
		if number == 15 {
			value, n := protowire.ConsumeVarint(unknown)
			if n < 0 {
				return fmt.Errorf("invalid inventory compression field")
			}
			if value != 0 {
				return fmt.Errorf("%w: compressed CRIU pages", errUnsupportedManifest)
			}
			unknown = unknown[n:]
			continue
		}
		n := protowire.ConsumeFieldValue(number, wire, unknown)
		if n < 0 {
			return fmt.Errorf("invalid inventory protobuf extension")
		}
		unknown = unknown[n:]
	}
	return nil
}

func addImages(root string, manifest *Manifest) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".img") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		manifest.Images[entry.Name()] = Image{URI: entry.Name(), Size: uint64(info.Size())}
	}
	return nil
}

var mmImagePattern = regexp.MustCompile(`^mm-([0-9]+)\.img$`)

type sharedPlan struct {
	shmid     uint64
	length    uint64
	semantics string
}

func addMemoryObjects(root string, manifest *Manifest) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var mmNames []string
	for _, entry := range entries {
		if mmImagePattern.MatchString(entry.Name()) {
			mmNames = append(mmNames, entry.Name())
		}
	}
	sort.Strings(mmNames)
	shared := make(map[uint64]sharedPlan)
	nextID := uint64(1)
	for _, name := range mmNames {
		match := mmImagePattern.FindStringSubmatch(name)
		pid64, _ := strconv.ParseUint(match[1], 10, 32)
		pid := uint32(pid64)
		mmImage, err := decodeImage(filepath.Join(root, name), &mm.MmEntry{})
		if err != nil {
			return err
		}
		if len(mmImage.Entries) != 1 {
			return fmt.Errorf("invalid %s: got %d entries", name, len(mmImage.Entries))
		}
		mmEntry := mmImage.Entries[0].Message.(*mm.MmEntry)
		reader, err := crit.NewMemoryReader(root, pid, os.Getpagesize())
		if err != nil {
			return err
		}
		pageImage := fmt.Sprintf("pages-%d.img", reader.GetPagesID())
		if _, ok := manifest.Images[pageImage]; !ok {
			return fmt.Errorf("pagemap-%d.img references missing %s", pid, pageImage)
		}
		pageRanges, err := rawPageRanges(reader.GetPagemapEntries(), pageImage, uint64(os.Getpagesize()))
		if err != nil {
			return fmt.Errorf("pagemap-%d.img: %w", pid, err)
		}
		for vmaID, area := range mmEntry.GetVmas() {
			if eligiblePrivate(area) {
				object := HostMemoryObject{
					MemoryID:  nextID,
					Name:      fmt.Sprintf("private-%d-%d", pid, vmaID),
					PID:       pid,
					VMAID:     uint32(vmaID),
					DstAddr:   area.GetStart(),
					Length:    area.GetEnd() - area.GetStart(),
					Semantics: "private_anon",
					MapMode:   "private",
				}
				object.SourceRange = intersectRanges(pageRanges, area.GetStart(), area.GetEnd())
				manifest.HostMemoryObjects = append(manifest.HostMemoryObjects, object)
				nextID++
			}
			if area.GetStatus()&vmaAnonShared != 0 {
				length := area.GetPgoff() + area.GetEnd() - area.GetStart()
				if length > shared[area.GetShmid()].length {
					shared[area.GetShmid()] = sharedPlan{
						shmid: area.GetShmid(), length: length, semantics: "shared_anon",
					}
				}
			}
		}
	}
	if err := addMemfdPlans(root, shared); err != nil {
		return err
	}
	var shmids []uint64
	for shmid := range shared {
		shmids = append(shmids, shmid)
	}
	sort.Slice(shmids, func(i, j int) bool { return shmids[i] < shmids[j] })
	for _, shmid := range shmids {
		plan := shared[shmid]
		var ranges []SourceRange
		pagemapName := fmt.Sprintf("pagemap-shmem-%d.img", shmid)
		if plan.length != 0 {
			image, err := decodeImage(filepath.Join(root, pagemapName), &pagemap.PagemapHead{})
			if err != nil {
				return err
			}
			if len(image.Entries) == 0 {
				return fmt.Errorf("invalid %s: missing head", pagemapName)
			}
			head := image.Entries[0].Message.(*pagemap.PagemapHead)
			pageImage := fmt.Sprintf("pages-%d.img", head.GetPagesId())
			if _, ok := manifest.Images[pageImage]; !ok {
				return fmt.Errorf("%s references missing %s", pagemapName, pageImage)
			}
			var entries []*pagemap.PagemapEntry
			for _, entry := range image.Entries[1:] {
				entries = append(entries, entry.Message.(*pagemap.PagemapEntry))
			}
			ranges, err = rawPageRanges(entries, pageImage, uint64(os.Getpagesize()))
			if err != nil {
				return fmt.Errorf("%s: %w", pagemapName, err)
			}
		}
		object := HostMemoryObject{
			MemoryID:    nextID,
			Name:        fmt.Sprintf("shared-%d", shmid),
			Shmid:       shmid,
			Length:      plan.length,
			Semantics:   plan.semantics,
			MapMode:     "shared",
			SourceRange: intersectRanges(ranges, 0, plan.length),
		}
		manifest.HostMemoryObjects = append(manifest.HostMemoryObjects, object)
		nextID++
	}
	return nil
}

func addMemfdPlans(root string, shared map[uint64]sharedPlan) error {
	path := filepath.Join(root, "memfd.img")
	image, err := decodeImage(path, &memfd.MemfdInodeEntry{})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range image.Entries {
		inode := entry.Message.(*memfd.MemfdInodeEntry)
		if inode.HugetlbFlag != nil && inode.GetHugetlbFlag() != 0 {
			return fmt.Errorf("%w: hugetlb memfd shmid %d", errUnsupportedManifest, inode.GetShmid())
		}
		if inode.GetSize() == 0 {
			return fmt.Errorf("%w: zero-length memfd shmid %d", errUnsupportedManifest, inode.GetShmid())
		}
		shmid := uint64(inode.GetShmid())
		if existing, ok := shared[shmid]; ok && existing.semantics != "shared_memfd" {
			return fmt.Errorf("shared-memory id %d is both anonymous and memfd-backed", shmid)
		}
		shared[shmid] = sharedPlan{
			shmid: shmid, length: inode.GetSize(), semantics: "shared_memfd",
		}
	}
	return nil
}

func eligiblePrivate(area *vma.VmaEntry) bool {
	return area.GetStatus()&vmaAnonPrivate != 0 && area.GetFlags()&mapGrowsDown == 0 && area.GetMadv()&(uint64(1)<<madvWipeOnFork) == 0 && area.GetEnd() > area.GetStart()
}

func rawPageRanges(entries []*pagemap.PagemapEntry, object string, pageSize uint64) ([]SourceRange, error) {
	var offset uint64
	var ranges []SourceRange
	for _, entry := range entries {
		flags := entry.GetFlags()
		if entry.GetInParent() || flags&peParent != 0 {
			return nil, fmt.Errorf("%w: incremental parent pages", errUnsupportedManifest)
		}
		if flags&^(peLazy|pePresent|pePayloadAlign) != 0 || flags != 0 && flags&pePresent == 0 {
			return nil, fmt.Errorf("%w: unsupported pagemap flags %#x", errUnsupportedManifest, flags)
		}
		if len(entry.ProtoReflect().GetUnknown()) != 0 {
			return nil, fmt.Errorf("%w: compressed or unknown pagemap encoding", errUnsupportedManifest)
		}
		length := entry.GetNrPages() * pageSize
		if length == 0 {
			length = uint64(entry.GetCompatNrPages()) * pageSize
		}
		if length == 0 {
			continue
		}
		if flags&pePayloadAlign != 0 {
			offset = (offset + pageSize - 1) / pageSize * pageSize
		}
		ranges = append(ranges, SourceRange{Object: object, SourceOffset: offset, DstOffset: entry.GetVaddr(), Length: length})
		offset += length
	}
	return ranges, nil
}

func intersectRanges(ranges []SourceRange, start, end uint64) []SourceRange {
	var result []SourceRange
	for _, source := range ranges {
		rangeStart := source.DstOffset
		rangeEnd := rangeStart + source.Length
		intersectionStart := max(start, rangeStart)
		intersectionEnd := min(end, rangeEnd)
		if intersectionStart >= intersectionEnd {
			continue
		}
		result = append(result, SourceRange{
			Object:       source.Object,
			SourceOffset: source.SourceOffset + intersectionStart - rangeStart,
			DstOffset:    intersectionStart - start,
			Length:       intersectionEnd - intersectionStart,
		})
	}
	return result
}
