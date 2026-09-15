// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

//! Defined-symbol discovery from an ELF64 little-endian image. This implements
//! run-ai's scoped object lookup, not glibc namespace/version/IFUNC semantics.
//! All file offsets are checked before access; no mapped ELF bytes become refs
//! to Rust structures.

use std::ffi::{CStr, c_void};

fn integer<const N: usize>(data: &[u8], offset: usize) -> Option<[u8; N]> {
    data.get(offset..offset.checked_add(N)?)?.try_into().ok()
}

// ELF64 field locations belong to these checked readers, never the resolver.
// Reading integers into native values avoids alignment and aliasing assumptions.
const ELF_IDENT: &[u8] = b"\x7fELF\x02\x01\x01";
const ELF_HEADER_SECTION_OFFSET: usize = 40;
const ELF_HEADER_SECTION_ENTRY_SIZE: usize = 58;
const ELF_HEADER_SECTION_COUNT: usize = 60;
const SECTION_SIZE: usize = 64;
const SECTION_TYPE: usize = 4;
const SECTION_OFFSET: usize = 24;
const SECTION_LENGTH: usize = 32;
const SECTION_LINK: usize = 40;
const SECTION_ENTRY_SIZE: usize = 56;
const SYMBOL_SIZE: usize = 24;
const SYMBOL_NAME: usize = 0;
const SYMBOL_INFO: usize = 4;
const SYMBOL_VISIBILITY: usize = 5;
const SYMBOL_SECTION: usize = 6;
const SYMBOL_VALUE: usize = 8;
const SHT_DYNSYM: u32 = 11;
const SHT_STRTAB: u32 = 3;
const SHN_UNDEF: u16 = 0;
const STT_GNU_IFUNC: u8 = 10;

struct ElfHeader {
    sections_offset: usize,
    section_entry_size: usize,
    section_count: usize,
}

impl ElfHeader {
    fn read(data: &[u8]) -> Option<Self> {
        if data.get(..ELF_IDENT.len())? != ELF_IDENT {
            return None;
        }
        let header = Self {
            sections_offset: u64::from_le_bytes(integer(data, ELF_HEADER_SECTION_OFFSET)?) as usize,
            section_entry_size: u16::from_le_bytes(integer(data, ELF_HEADER_SECTION_ENTRY_SIZE)?)
                as usize,
            section_count: u16::from_le_bytes(integer(data, ELF_HEADER_SECTION_COUNT)?) as usize,
        };
        if header.section_entry_size < SECTION_SIZE || header.section_count == 0 {
            return None;
        }
        Some(header)
    }

    fn section(&self, data: &[u8], index: usize) -> Option<Section> {
        if index >= self.section_count {
            return None;
        }
        let offset = self
            .sections_offset
            .checked_add(index.checked_mul(self.section_entry_size)?)?;
        let bytes = data.get(offset..offset.checked_add(SECTION_SIZE)?)?;
        Some(Section {
            kind: u32::from_le_bytes(integer(bytes, SECTION_TYPE)?),
            offset: u64::from_le_bytes(integer(bytes, SECTION_OFFSET)?) as usize,
            length: u64::from_le_bytes(integer(bytes, SECTION_LENGTH)?) as usize,
            link: u32::from_le_bytes(integer(bytes, SECTION_LINK)?) as usize,
            entry_size: u64::from_le_bytes(integer(bytes, SECTION_ENTRY_SIZE)?) as usize,
        })
    }
}

#[cfg(test)]
mod reader_tests {
    use super::*;

    #[test]
    fn checked_readers_find_symbols_and_reject_bad_tables() {
        // Minimal independent ELF64 fixture: header, null/dynsym/strtab
        // sections, one symbol, and its name. No native struct casts.
        let mut bytes = [0u8; 320];
        bytes[..7].copy_from_slice(ELF_IDENT);
        bytes[40..48].copy_from_slice(&64u64.to_le_bytes());
        bytes[58..60].copy_from_slice(&64u16.to_le_bytes());
        bytes[60..62].copy_from_slice(&3u16.to_le_bytes());
        bytes[132..136].copy_from_slice(&11u32.to_le_bytes());
        bytes[152..160].copy_from_slice(&256u64.to_le_bytes());
        bytes[160..168].copy_from_slice(&24u64.to_le_bytes());
        bytes[168..172].copy_from_slice(&2u32.to_le_bytes());
        bytes[184..192].copy_from_slice(&24u64.to_le_bytes());
        bytes[196..200].copy_from_slice(&3u32.to_le_bytes());
        bytes[216..224].copy_from_slice(&280u64.to_le_bytes());
        bytes[224..232].copy_from_slice(&8u64.to_le_bytes());
        bytes[256..260].copy_from_slice(&1u32.to_le_bytes());
        bytes[260] = 0x12; // STB_GLOBAL | STT_FUNC
        bytes[262..264].copy_from_slice(&1u16.to_le_bytes());
        bytes[264..272].copy_from_slice(&0x1234u64.to_le_bytes());
        bytes[281..288].copy_from_slice(b"lookup\0");
        let symbol = defined_symbol(&bytes, b"lookup").unwrap();
        assert_eq!(symbol.value, 0x1234);
        assert!(!symbol.indirect);
        bytes[260] = 0x1a; // STT_GNU_IFUNC
        assert!(defined_symbol(&bytes, b"lookup").unwrap().indirect);
        assert!(defined_symbol(&bytes, b"absent").is_none());
        for (offset, value) in [(184, 0u64), (152, u64::MAX), (216, u64::MAX)] {
            let mut bad = bytes;
            bad[offset..offset + 8].copy_from_slice(&value.to_le_bytes());
            assert!(defined_symbol(&bad, b"lookup").is_none());
        }
        for length in 0..288 {
            assert!(defined_symbol(&bytes[..length], b"lookup").is_none());
        }
    }
}

struct Section {
    kind: u32,
    offset: usize,
    length: usize,
    link: usize,
    entry_size: usize,
}

struct SymbolEntry {
    name_offset: usize,
    kind: u8,
    binding: u8,
    visibility: u8,
    section: u16,
    value: usize,
}

impl SymbolEntry {
    fn read(bytes: &[u8]) -> Option<Self> {
        if bytes.len() < SYMBOL_SIZE {
            return None;
        }
        Some(Self {
            name_offset: u32::from_le_bytes(integer(bytes, SYMBOL_NAME)?) as usize,
            kind: bytes[SYMBOL_INFO] & 15,
            binding: bytes[SYMBOL_INFO] >> 4,
            visibility: bytes[SYMBOL_VISIBILITY] & 3,
            section: u16::from_le_bytes(integer(bytes, SYMBOL_SECTION)?),
            value: u64::from_le_bytes(integer(bytes, SYMBOL_VALUE)?) as usize,
        })
    }
}

pub struct Symbol {
    pub value: usize,
    pub indirect: bool,
}

pub fn defined_symbol(data: &[u8], name: &[u8]) -> Option<Symbol> {
    let elf = ElfHeader::read(data)?;
    for index in 0..elf.section_count {
        let table = elf.section(data, index)?;
        if table.kind != SHT_DYNSYM {
            continue;
        }
        if table.entry_size < SYMBOL_SIZE || table.length % table.entry_size != 0 {
            return None;
        }
        let symbols = data.get(table.offset..table.offset.checked_add(table.length)?)?;
        let strings = elf.section(data, table.link)?;
        if strings.kind != SHT_STRTAB {
            return None;
        }
        let strings = data.get(strings.offset..strings.offset.checked_add(strings.length)?)?;
        for entry in symbols.chunks_exact(table.entry_size) {
            let symbol = SymbolEntry::read(entry)?;
            if symbol.section == SHN_UNDEF
                || !matches!(symbol.binding, 1 | 2)
                || !matches!(symbol.visibility, 0 | 3)
                || !matches!(symbol.kind, 0..=2 | STT_GNU_IFUNC)
            {
                continue;
            }
            let tail = strings.get(symbol.name_offset..)?;
            let end = tail.iter().position(|byte| *byte == 0)?;
            if &tail[..end] == name {
                return Some(Symbol {
                    value: symbol.value,
                    indirect: symbol.kind == STT_GNU_IFUNC,
                });
            }
        }
    }
    None
}

/// Bootstrap cannot allocate through malloc: sanitizers and allocator
/// interposers themselves resolve symbols before their allocator is ready.
pub struct Image {
    address: *mut c_void,
    size: usize,
}

impl Image {
    pub fn open(path: &CStr) -> Option<Self> {
        let fd = unsafe {
            libc::syscall(
                libc::SYS_openat,
                libc::AT_FDCWD,
                path.as_ptr(),
                libc::O_RDONLY | libc::O_CLOEXEC,
                0,
            )
        } as i32;
        if fd < 0 {
            return None;
        }
        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
        if unsafe { libc::syscall(libc::SYS_fstat, fd, &mut stat) } != 0
            || stat.st_size <= 0
            || stat.st_size as u64 > isize::MAX as u64
        {
            unsafe {
                libc::syscall(libc::SYS_close, fd);
            }
            return None;
        }
        let size = stat.st_size as usize;
        let address = unsafe {
            libc::syscall(
                libc::SYS_mmap,
                0usize,
                size,
                libc::PROT_READ,
                libc::MAP_PRIVATE,
                fd,
                0usize,
            )
        } as *mut c_void;
        unsafe {
            libc::syscall(libc::SYS_close, fd);
        }
        if address == libc::MAP_FAILED {
            return None;
        }
        Some(Self { address, size })
    }

    pub fn symbol(&self, name: &[u8]) -> Option<Symbol> {
        // SAFETY: the read-only mapping remains live until Image is dropped.
        defined_symbol(
            unsafe { std::slice::from_raw_parts(self.address.cast::<u8>(), self.size) },
            name,
        )
    }
}

impl Drop for Image {
    fn drop(&mut self) {
        unsafe {
            libc::syscall(libc::SYS_munmap, self.address, self.size);
        }
    }
}

struct Enumeration<'a> {
    caller: usize,
    found: bool,
    name: &'a CStr,
    result: *mut c_void,
    failed: bool,
}

// SAFETY: dl_iterate_phdr owns the headers for the duration of the callback.
// Panic containment must occur *inside* this C ABI callback, not around the
// outer dl_iterate_phdr call.
unsafe extern "C" fn collect(info: *mut libc::dl_phdr_info, _: usize, data: *mut c_void) -> i32 {
    let context = unsafe { &mut *data.cast::<Enumeration<'_>>() };
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let info = unsafe { &*info };
        let bias = info.dlpi_addr as usize;
        if !context.found {
            for header in
                unsafe { std::slice::from_raw_parts(info.dlpi_phdr, info.dlpi_phnum as usize) }
            {
                if header.p_type == libc::PT_LOAD {
                    let start = bias.wrapping_add(header.p_vaddr as usize);
                    if context.caller >= start && context.caller - start < header.p_memsz as usize {
                        context.found = true;
                        break;
                    }
                }
            }
        } else {
            let path = unsafe { CStr::from_ptr(info.dlpi_name) };
            if !path.to_bytes().is_empty()
                && path.to_bytes() != b"linux-vdso.so.1"
                && let Some(image) = Image::open(path)
                && let Some(symbol) = image.symbol(context.name.to_bytes())
                && let Some(address) = bias.checked_add(symbol.value)
            {
                context.result = if symbol.indirect {
                    // GNU IFUNC st_value names a resolver, not the
                    // implementation (notably glibc memcpy/memset).
                    let resolver: unsafe extern "C" fn() -> *mut c_void =
                        unsafe { std::mem::transmute(address) };
                    unsafe { resolver() }
                } else {
                    address as *mut c_void
                };
            }
        }
    }));
    if let Err(payload) = result {
        std::mem::forget(payload);
        context.failed = true;
        return 1;
    }
    i32::from(!context.result.is_null())
}

pub fn after(caller: *const c_void, name: &CStr) -> *mut c_void {
    let mut context = Enumeration {
        caller: caller as usize,
        found: false,
        name,
        result: std::ptr::null_mut(),
        failed: false,
    };
    unsafe {
        libc::dl_iterate_phdr(Some(collect), (&mut context as *mut Enumeration<'_>).cast());
    }
    if context.failed {
        return std::ptr::null_mut();
    }
    context.result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn malformed_images_do_not_panic() {
        for length in 0..1024 {
            let mut bytes = vec![0xff; length];
            if length >= 7 {
                bytes[..7].copy_from_slice(b"\x7fELF\x02\x01\x01");
            }
            assert!(defined_symbol(&bytes, b"dlsym").is_none());
        }
    }
}
