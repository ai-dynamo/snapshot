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
    let other = rmp_serde::to_vec_named(&Message {
        version: VERSION + 1,
        body: Request::Inspect { namespace_pid: 1 },
    })
    .unwrap();
    assert!(decode::<Request>(&other).is_err());
    let mut bytes = encode(&Request::Inspect { namespace_pid: 1 }).unwrap();
    bytes.push(0);
    assert!(decode::<Request>(&bytes).is_err());
}

#[test]
fn virtual_shareable_handle_codec_owns_the_fixed_layout() {
    let reference = AllocationReference {
        id: [0x22; 16],
        creator_pid: 0x11223344,
    };
    let encoded = encode_virtual_shareable_handle(reference).unwrap();
    assert_eq!(
        encoded,
        [
            b'C', b'U', b'I', VERSION, 0x44, 0x33, 0x22, 0x11, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22,
            0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22,
        ]
    );
    assert_eq!(
        decode_virtual_shareable_handle(&encoded).unwrap(),
        reference
    );
    let mut unsupported = encoded;
    unsupported[3] = VERSION + 1;
    assert!(decode_virtual_shareable_handle(&unsupported).is_err());
    let mut invalid = encoded;
    invalid[4..8].fill(0);
    assert!(decode_virtual_shareable_handle(&invalid).is_err());
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
    // The peer reports EOF only after every SCM_RIGHTS duplicate closes, so this
    // detects descriptor leaks on decoding failure, excess descriptors, and a truncated
    // ancillary buffer.
    for count in [1, 2, 8] {
        let (reader, writer) = UnixStream::pair().unwrap();
        reader
            .set_read_timeout(Some(std::time::Duration::from_secs(1)))
            .unwrap();
        let (sender, receiver) = UnixStream::pair().unwrap();
        let mut space = [std::mem::MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(8))];
        let mut ancillary = rustix::net::SendAncillaryBuffer::new(&mut space);
        let descriptors = [writer.as_fd(); 8];
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
        let error = receive::<Request>(&receiver).unwrap_err();
        match count {
            1 => assert!(matches!(error, Error::Decode(_))),
            2 => assert!(matches!(
                error,
                Error::Invalid("control socket received excess descriptors")
            )),
            8 => assert!(
                matches!(
                    error,
                    Error::Invalid("control socket ancillary data truncated")
                ),
                "{error:?}"
            ),
            _ => unreachable!(),
        }
        assert_eq!((&reader).read(&mut [0]).unwrap(), 0);
    }
}

#[test]
fn connect_times_out_when_the_peer_backlog_is_full() {
    use rustix::net::{AddressFamily, SocketAddrUnix, SocketType, bind, listen, socket};
    use std::time::{Duration, Instant};
    let path =
        std::env::temp_dir().join(format!("cuinterpose-backlog-{}.sock", std::process::id()));
    let listener = socket(AddressFamily::UNIX, SocketType::STREAM, None).unwrap();
    bind(&listener, &SocketAddrUnix::new(&path).unwrap()).unwrap();
    listen(&listener, 0).unwrap();
    // Linux permits backlog + 1 pending connections.
    let first = cuinterpose_protocol::connect(&path, Duration::from_millis(100)).unwrap();
    let started = Instant::now();
    let error = cuinterpose_protocol::connect(&path, Duration::from_millis(100)).unwrap_err();
    assert_eq!(error.kind(), std::io::ErrorKind::WouldBlock);
    assert!(started.elapsed() >= Duration::from_millis(50));
    assert!(started.elapsed() < Duration::from_secs(2));
    drop(first);
    std::fs::remove_file(path).unwrap();
}
