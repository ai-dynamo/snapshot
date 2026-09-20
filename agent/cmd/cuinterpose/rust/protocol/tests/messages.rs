// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cuinterpose_protocol::*;
use std::fs::File;
use std::io::{Read, Write};
use std::os::fd::{AsFd, OwnedFd};
use std::os::unix::net::UnixStream;

#[test]
fn versions_and_trailing_data_are_rejected() {
    #[derive(serde::Serialize)]
    struct Message {
        version: u8,
        body: Request,
    }
    let old = rmp_serde::to_vec_named(&Message {
        version: VERSION - 1,
        body: Request::Inspect { namespace_pid: 1 },
    })
    .unwrap();
    assert!(decode::<Request>(&old).is_err());
    let mut bytes = encode(&Request::Inspect { namespace_pid: 1 }).unwrap();
    bytes.push(0);
    assert!(decode::<Request>(&bytes).is_err());
}

#[test]
fn virtual_shareable_handle_codec_owns_the_fixed_layout() {
    assert_eq!(VIRTUAL_SHAREABLE_HANDLE_MAGIC, [b'C', b'U', b'I', 2]);
    assert_eq!(VIRTUAL_SHAREABLE_HANDLE_BYTES, 24);
    let reference = AllocationReference {
        id: [0x22; 16],
        creator_pid: 0x11223344,
    };
    let encoded = encode_virtual_shareable_handle(reference).unwrap();
    assert_eq!(
        encoded,
        [
            b'C', b'U', b'I', 2, 0x44, 0x33, 0x22, 0x11, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22,
            0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22,
        ]
    );
    assert_eq!(
        decode_virtual_shareable_handle(&encoded).unwrap(),
        reference
    );
    let mut obsolete = encoded;
    obsolete[3] = VERSION - 1;
    assert!(decode_virtual_shareable_handle(&obsolete).is_err());
    let mut invalid = encoded;
    invalid[4..8].fill(0);
    assert!(decode_virtual_shareable_handle(&invalid).is_err());
}

#[test]
fn inspection_metadata_round_trips() {
    let mapping = Record::Mapping {
        allocation: AllocationReference {
            id: [1; 16],
            creator_pid: 2,
        },
        address: 0x10000,
        size: 8192,
        offset: 4096,
        access: vec![(1, 0, 3), (1, 1, 3)],
    };
    let reply = Reply::Inspection {
        records: vec![mapping.clone()],
    };
    let Reply::Inspection { records, .. } = decode(&encode(&reply).unwrap()).unwrap() else {
        panic!("decoded the wrong reply variant");
    };
    assert_eq!(records, vec![mapping]);
}

#[test]
fn socket_preserves_frames_and_transfers_an_owned_cloexec_fd() {
    let (sender, receiver) = UnixStream::pair().unwrap();
    let fd: OwnedFd = File::open("/dev/zero").unwrap().into();
    let request = Request::Inspect { namespace_pid: 1 };
    send(&sender, &request, Some(&fd)).unwrap();
    send(&sender, &request, None).unwrap();
    let (message, received): (Request, _) = receive(&receiver).unwrap();
    assert!(matches!(message, Request::Inspect { namespace_pid: 1 }));
    let received = received.unwrap();
    assert!(
        rustix::io::fcntl_getfd(&received)
            .unwrap()
            .contains(rustix::io::FdFlags::CLOEXEC)
    );
    let mut bytes = [1; 4];
    File::from(received).read_exact(&mut bytes).unwrap();
    assert_eq!(bytes, [0; 4]);
    assert!(receive::<Request>(&receiver).unwrap().1.is_none());
}

#[test]
fn fragmented_prefix_and_body_are_accepted_and_oversized_prefix_is_refused() {
    let (mut sender, receiver) = UnixStream::pair().unwrap();
    let bytes = encode(&Request::Inspect { namespace_pid: 1 }).unwrap();
    let worker = std::thread::spawn(move || {
        for byte in (bytes.len() as u32).to_le_bytes().into_iter().chain(bytes) {
            sender.write_all(&[byte]).unwrap();
        }
    });
    assert!(matches!(
        receive::<Request>(&receiver).unwrap().0,
        Request::Inspect { namespace_pid: 1 }
    ));
    worker.join().unwrap();
    let (mut sender, receiver) = UnixStream::pair().unwrap();
    sender
        .write_all(&((MAX_MESSAGE_BYTES + 1) as u32).to_le_bytes())
        .unwrap();
    assert!(receive::<Request>(&receiver).is_err());
}

#[test]
fn malformed_or_excess_ancillary_data_closes_received_descriptors() {
    // The peer reports EOF only when every SCM_RIGHTS duplicate has closed.
    // Cover decoding failure, excess rights, and ancillary-buffer truncation.
    for count in [1, 2, 4] {
        let (reader, writer) = UnixStream::pair().unwrap();
        reader
            .set_read_timeout(Some(std::time::Duration::from_secs(1)))
            .unwrap();
        let (sender, receiver) = UnixStream::pair().unwrap();
        let mut space = [std::mem::MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(4))];
        let mut ancillary = rustix::net::SendAncillaryBuffer::new(&mut space);
        let descriptors = [writer.as_fd(); 4];
        ancillary.push(rustix::net::SendAncillaryMessage::ScmRights(
            &descriptors[..count],
        ));
        let body = if count == 1 {
            vec![0xc1]
        } else {
            encode(&Request::Inspect { namespace_pid: 1 }).unwrap()
        };
        let frame = [(body.len() as u32).to_le_bytes().as_slice(), &body].concat();
        rustix::net::sendmsg(
            &sender,
            &[std::io::IoSlice::new(&frame)],
            &mut ancillary,
            rustix::net::SendFlags::NOSIGNAL,
        )
        .unwrap();
        drop(writer);
        assert!(receive::<Request>(&receiver).is_err());
        assert_eq!((&reader).read(&mut [0]).unwrap(), 0);
    }
}
