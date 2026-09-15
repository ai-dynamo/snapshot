// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cuinterpose_protocol::*;
use std::fs::File;
use std::io::{Read, Write};
use std::os::fd::{AsFd, OwnedFd};
use std::os::unix::net::UnixStream;

#[test]
fn identities_are_binary_on_wire_and_hex_only_at_configuration_boundary() {
    let id: ParticipantId = "0123456789abcdef0123456789abcdef".parse().unwrap();
    assert_eq!(id.to_string(), "0123456789abcdef0123456789abcdef");
    assert_eq!(decode::<ParticipantId>(&encode(&id).unwrap()).unwrap(), id);
    for invalid in ["", "A123456789abcdef0123456789abcdef0", "0123456789abcdef"] {
        assert!(invalid.parse::<ParticipantId>().is_err());
    }
}

#[test]
fn versions_and_trailing_data_are_rejected() {
    #[derive(serde::Serialize)]
    struct Message {
        version: u16,
        body: Request,
    }
    let old = rmp_serde::to_vec_named(&Message {
        version: 2,
        body: Request::Handshake,
    })
    .unwrap();
    assert!(decode::<Request>(&old).is_err());
    let mut bytes = encode(&Request::Handshake).unwrap();
    bytes.push(0);
    assert!(decode::<Request>(&bytes).is_err());
}

#[test]
fn maximal_inspection_fits_and_excess_records_are_rejected() {
    let mapping = Record::Mapping {
        id: AllocationId([1; 16]),
        creator: true,
        address: u64::MAX,
        size: u64::MAX,
        offset: u64::MAX,
        access: (0..MAX_ACCESS)
            .map(|i| Access {
                location_type: i32::MAX,
                location_id: i as i32,
                flags: u64::MAX,
            })
            .collect(),
    };
    let reply = Reply::Inspection {
        records: vec![mapping.clone(); MAX_RECORDS],
        live_raw_imports: 0,
        unsupported_creations: 0,
    };
    let bytes = encode(&reply).unwrap();
    assert!(bytes.len() < MAX_BYTES);
    assert!(decode::<Reply>(&bytes).is_ok());
    let oversized = Reply::Inspection {
        records: vec![mapping; MAX_RECORDS + 1],
        live_raw_imports: 0,
        unsupported_creations: 0,
    };
    assert!(decode::<Reply>(&encode(&oversized).unwrap()).is_err());
}

#[test]
fn socket_preserves_frames_and_transfers_an_owned_cloexec_fd() {
    let (sender, receiver) = UnixStream::pair().unwrap();
    let fd: OwnedFd = File::open("/dev/zero").unwrap().into();
    send(&sender, &Request::Handshake, Some(&fd)).unwrap();
    send(&sender, &Request::Handshake, None).unwrap();
    let (message, received): (Request, _) = receive(&receiver).unwrap();
    assert!(matches!(message, Request::Handshake));
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
    let bytes = encode(&Request::Handshake).unwrap();
    let worker = std::thread::spawn(move || {
        for byte in (bytes.len() as u32).to_le_bytes().into_iter().chain(bytes) {
            sender.write_all(&[byte]).unwrap();
        }
    });
    assert!(matches!(
        receive::<Request>(&receiver).unwrap().0,
        Request::Handshake
    ));
    worker.join().unwrap();
    let (mut sender, receiver) = UnixStream::pair().unwrap();
    sender
        .write_all(&((MAX_BYTES + 1) as u32).to_le_bytes())
        .unwrap();
    assert!(receive::<Request>(&receiver).is_err());
}

#[test]
fn malformed_frame_closes_received_descriptors() {
    // A pipe reports EOF only when all write descriptors, including any leaked
    // SCM_RIGHTS duplicate, have closed.
    let (reader, writer) = UnixStream::pair().unwrap();
    reader
        .set_read_timeout(Some(std::time::Duration::from_secs(1)))
        .unwrap();
    let (sender, receiver) = UnixStream::pair().unwrap();
    let mut space = [std::mem::MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(1))];
    let mut ancillary = rustix::net::SendAncillaryBuffer::new(&mut space);
    let descriptors = [writer.as_fd()];
    ancillary.push(rustix::net::SendAncillaryMessage::ScmRights(&descriptors));
    rustix::net::sendmsg(
        &sender,
        &[std::io::IoSlice::new(&[1, 0, 0, 0, 0xc1])],
        &mut ancillary,
        rustix::net::SendFlags::NOSIGNAL,
    )
    .unwrap();
    drop(writer);
    assert!(receive::<Request>(&receiver).is_err());
    assert_eq!((&reader).read(&mut [0]).unwrap(), 0);
}
