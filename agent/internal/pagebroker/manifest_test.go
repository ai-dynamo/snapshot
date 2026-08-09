// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/inventory"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/memfd"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/mm"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pagemap"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/vma"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func writeCRIUImage(t *testing.T, path, magic string, messages ...proto.Message) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	image := &crit.CriuImage{Magic: magic}
	for _, message := range messages {
		image.Entries = append(image.Entries, &crit.CriuEntry{Message: message})
	}
	if err := crit.New(nil, file, "", false, true).Encode(image); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func mmFixture(vmas ...*vma.VmaEntry) *mm.MmEntry {
	zero := uint64(0)
	return &mm.MmEntry{
		MmStartCode:  &zero,
		MmEndCode:    &zero,
		MmStartData:  &zero,
		MmEndData:    &zero,
		MmStartStack: &zero,
		MmStartBrk:   &zero,
		MmBrk:        &zero,
		MmArgStart:   &zero,
		MmArgEnd:     &zero,
		MmEnvStart:   &zero,
		MmEnvEnd:     &zero,
		ExeFileId:    proto.Uint32(1),
		Vmas:         vmas,
	}
}

func vmaFixture(start, end, pgoff, shmid uint64, status uint32) *vma.VmaEntry {
	return &vma.VmaEntry{
		Start:  proto.Uint64(start),
		End:    proto.Uint64(end),
		Pgoff:  proto.Uint64(pgoff),
		Shmid:  proto.Uint64(shmid),
		Prot:   proto.Uint32(3),
		Flags:  proto.Uint32(0x22),
		Status: proto.Uint32(status),
		Fd:     proto.Int64(-1),
	}
}

func pagemapFixture(address uint64, pages uint64) *pagemap.PagemapEntry {
	return &pagemap.PagemapEntry{
		Vaddr:         proto.Uint64(address),
		CompatNrPages: proto.Uint32(uint32(pages)),
		NrPages:       proto.Uint64(pages),
	}
}

func writePageBytes(t *testing.T, path string, values ...byte) {
	t.Helper()
	page := make([]byte, os.Getpagesize())
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		for i := range page {
			page[i] = value
		}
		if _, err := file.Write(page); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateManifestMapsHolesMultipleVMAsAndSharedMemory(t *testing.T) {
	root := t.TempDir()
	page := uint64(os.Getpagesize())
	writeCRIUImage(t, filepath.Join(root, "inventory.img"), "INVENTORY",
		&inventory.InventoryEntry{ImgVersion: proto.Uint32(1)})
	writeCRIUImage(t, filepath.Join(root, "mm-11.img"), "MM",
		mmFixture(
			vmaFixture(page, 5*page, 0, 0, vmaAnonPrivate),
			vmaFixture(8*page, 9*page, 0, 0, vmaAnonPrivate),
			vmaFixture(10*page, 12*page, 0, 7, vmaAnonShared),
		))
	writeCRIUImage(t, filepath.Join(root, "pagemap-11.img"), "PAGEMAP",
		&pagemap.PagemapHead{PagesId: proto.Uint32(1)},
		pagemapFixture(page, 1),
		pagemapFixture(3*page, 1),
		pagemapFixture(8*page, 1),
	)
	writePageBytes(t, filepath.Join(root, "pages-1.img"), 'a', 'b', 'c')
	writeCRIUImage(t, filepath.Join(root, "pagemap-shmem-7.img"), "PAGEMAP",
		&pagemap.PagemapHead{PagesId: proto.Uint32(2)},
		pagemapFixture(0, 1),
	)
	writePageBytes(t, filepath.Join(root, "pages-2.img"), 's')
	writeCRIUImage(t, filepath.Join(root, "memfd.img"), "MEMFD_INODE",
		&memfd.MemfdInodeEntry{
			Name: proto.String("shared-buffer"), Size: proto.Uint64(2 * page),
			Shmid: proto.Uint32(9), Seals: proto.Uint32(1),
			Uid: proto.Uint32(0), Gid: proto.Uint32(0), InodeId: proto.Uint64(1),
		})
	writeCRIUImage(t, filepath.Join(root, "pagemap-shmem-9.img"), "PAGEMAP",
		&pagemap.PagemapHead{PagesId: proto.Uint32(3)},
		pagemapFixture(page, 1),
	)
	writePageBytes(t, filepath.Join(root, "pages-3.img"), 'm')

	manifest, err := GenerateManifest(root, "checkpoint-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.HostMemoryObjects) != 4 {
		t.Fatalf("host objects = %d, want two private and two shared", len(manifest.HostMemoryObjects))
	}
	first := manifest.HostMemoryObjects[0]
	if first.PID != 11 || first.VMAID != 0 || first.Length != 4*page || len(first.SourceRange) != 2 {
		t.Fatalf("first VMA = %#v", first)
	}
	if first.SourceRange[0].DstOffset != 0 || first.SourceRange[1].DstOffset != 2*page {
		t.Fatalf("hole mapping = %#v", first.SourceRange)
	}
	second := manifest.HostMemoryObjects[1]
	if second.VMAID != 1 || len(second.SourceRange) != 1 || second.SourceRange[0].SourceOffset != 2*page {
		t.Fatalf("second VMA = %#v", second)
	}
	shared := manifest.HostMemoryObjects[2]
	if shared.Shmid != 7 || shared.MapMode != "shared" || shared.Length != 2*page || len(shared.SourceRange) != 1 {
		t.Fatalf("shared object = %#v", shared)
	}
	sharedMemfd := manifest.HostMemoryObjects[3]
	if sharedMemfd.Shmid != 9 || sharedMemfd.Semantics != "shared_memfd" || sharedMemfd.Length != 2*page || len(sharedMemfd.SourceRange) != 1 || sharedMemfd.SourceRange[0].DstOffset != page {
		t.Fatalf("shared memfd object = %#v", sharedMemfd)
	}
	if manifest.ResidentBytes != 5*page {
		t.Fatalf("resident bytes = %d, want %d populated bytes", manifest.ResidentBytes, 5*page)
	}
	manifestYAML, err := os.ReadFile(filepath.Join(root, ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(manifestYAML, []byte("digest:")) {
		t.Fatalf("manifest unexpectedly contains payload digests")
	}
	readBack, err := ReadPageBrokerManifest(filepath.Join(root, ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if readBack.CheckpointID != "checkpoint-1" {
		t.Fatalf("checkpoint ID = %q", readBack.CheckpointID)
	}
}

func TestGenerateManifestRejectsParentAndCompressedPagemaps(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*pagemap.PagemapEntry)
	}{
		{name: "parent", mutate: func(entry *pagemap.PagemapEntry) { entry.InParent = proto.Bool(true) }},
		{name: "compressed", mutate: func(entry *pagemap.PagemapEntry) {
			entry.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 6, protowire.VarintType), 1))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			page := uint64(os.Getpagesize())
			writeCRIUImage(t, filepath.Join(root, "inventory.img"), "INVENTORY",
				&inventory.InventoryEntry{ImgVersion: proto.Uint32(1)})
			writeCRIUImage(t, filepath.Join(root, "mm-1.img"), "MM",
				mmFixture(vmaFixture(page, 2*page, 0, 0, vmaAnonPrivate)))
			entry := pagemapFixture(page, 1)
			test.mutate(entry)
			writeCRIUImage(t, filepath.Join(root, "pagemap-1.img"), "PAGEMAP",
				&pagemap.PagemapHead{PagesId: proto.Uint32(1)}, entry)
			writePageBytes(t, filepath.Join(root, "pages-1.img"), 'x')
			_, err := GenerateManifest(root, "checkpoint")
			if err == nil || !IsUnsupported(err) {
				t.Fatalf("error = %v, want unsupported checkpoint", err)
			}
		})
	}
}
