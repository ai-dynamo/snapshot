// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

use cuinterpose_protocol::*;
use rustix::net::{SendAncillaryBuffer, SendAncillaryMessage, SendFlags, sendmsg};
use std::fs::File;
use std::io::{IoSlice, Read, Write};
use std::mem::MaybeUninit;
use std::os::fd::{AsFd, BorrowedFd, OwnedFd};
use std::os::unix::net::UnixStream;

fn invalid<T>(result: Result<T>) -> &'static str {
    match result {
        Err(Error::Invalid(message)) => message,
        Err(error) => panic!("unexpected error {error:?}"),
        Ok(_) => panic!("accepted"),
    }
}

fn send_with_descriptors(socket: &UnixStream, bytes: &[u8], descriptors: &[BorrowedFd]) {
    let mut space = [MaybeUninit::uninit(); rustix::cmsg_space!(ScmRights(8))];
    let mut ancillary = SendAncillaryBuffer::new(&mut space);
    ancillary.push(SendAncillaryMessage::ScmRights(descriptors));
    sendmsg(
        socket,
        &[IoSlice::new(bytes)],
        &mut ancillary,
        SendFlags::NOSIGNAL,
    )
    .unwrap();
}

fn frame(body: &[u8]) -> Vec<u8> {
    [(body.len() as u32).to_le_bytes().as_slice(), body].concat()
}

#[test]
fn decode_rejects_unknown_version_and_trailing_bytes() {
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
    assert_eq!(
        invalid(decode::<Request>(&other)),
        "unsupported version or trailing message data"
    );
    let mut bytes = encode(&Request::Inspect { namespace_pid: 1 }).unwrap();
    bytes.push(0);
    assert_eq!(
        invalid(decode::<Request>(&bytes)),
        "unsupported version or trailing message data"
    );
}

#[test]
fn encoding_keeps_the_request_and_state_wire_format() {
    // Field and variant names are encoded, so renaming one makes existing state unreadable.
    let request = Request::Execute {
        namespace_pid: 1,
        operation: Operation::SaveAllocations,
    };
    assert_eq!(
        encode(&request).unwrap(),
        b"\x82\xa7version\x01\xa4body\x83\xa4kind\xa7execute\xadnamespace_pid\x01\
          \xa9operation\xb0save_allocations"
    );
    let state = Manifest::from([(
        1,
        vec![Record::Allocation {
            allocation: AllocationReference {
                id: *b"allocation-id-01",
                creator_pid: 1,
            },
            shared: true,
            peer_access_granted: false,
            size: 4096,
            allocation_type: 1,
            handle_types: 1,
            location: MemoryLocation {
                location_type: 1,
                id: 0,
            },
            virtual_allocation_handle_count: 1,
        }],
    )]);
    assert_eq!(
        encode(&state).unwrap(),
        b"\x82\xa7version\x01\xa4body\x81\x01\x91\x81\xaaallocation\x88\
          \xaaallocation\x82\xa2id\xc4\x10allocation-id-01\xabcreator_pid\x01\
          \xa6shared\xc3\xb3peer_access_granted\xc2\xa4size\xcd\x10\x00\
          \xafallocation_type\x01\xachandle_types\x01\
          \xa8location\x82\xadlocation_type\x01\xa2id\x00\
          \xbfvirtual_allocation_handle_count\x01"
    );
}

#[test]
fn virtual_shareable_handle_has_fixed_layout_and_rejects_bad_version_or_zero_pid() {
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
    assert_eq!(
        invalid(decode_virtual_shareable_handle(&unsupported)),
        "invalid virtual shareable handle magic"
    );
    let mut invalid_pid = encoded;
    invalid_pid[4..8].fill(0);
    assert_eq!(
        invalid(decode_virtual_shareable_handle(&invalid_pid)),
        "invalid creator namespace PID"
    );
}

#[test]
fn send_receive_keeps_message_boundaries_and_passes_fd_as_cloexec() {
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
fn receive_reassembles_frame_split_at_ancillary_data_boundaries() {
    let (sender, receiver) = UnixStream::pair().unwrap();
    let frame = frame(&encode(&Request::Inspect { namespace_pid: 1 }).unwrap());
    let fd: OwnedFd = File::open("/dev/null").unwrap().into();
    // unix(7): a stream read stops after data that carries ancillary data. The prefix
    // arrives as frame[..1] and frame[1..4], as after a partial sendmsg in send, and the
    // body as frame[4..7] and frame[7..]. read has no control buffer, so the kernel closes
    // the second descriptor.
    send_with_descriptors(&sender, &frame[..1], &[fd.as_fd()]);
    (&sender).write_all(&frame[1..6]).unwrap();
    send_with_descriptors(&sender, &frame[6..7], &[fd.as_fd()]);
    (&sender).write_all(&frame[7..]).unwrap();
    drop(sender);
    let (message, received) = receive::<Request>(&receiver).unwrap();
    assert!(matches!(message, Request::Inspect { namespace_pid: 1 }));
    assert!(received.is_some());
}

#[test]
fn receive_rejects_oversized_length_prefix_before_reading_body() {
    let (mut sender, receiver) = UnixStream::pair().unwrap();
    sender
        .write_all(&((MAX_MESSAGE_BYTES + 1) as u32).to_le_bytes())
        .unwrap();
    // Closing the sender makes a missing limit check fail at EOF instead of blocking.
    drop(sender);
    assert_eq!(
        invalid(receive::<Request>(&receiver)),
        "message exceeds size limit"
    );
}

#[test]
fn receive_closes_passed_fds_on_decode_error_excess_fds_or_truncated_ancillary_data() {
    // The peer reports EOF only after every SCM_RIGHTS duplicate closes, so this
    // detects descriptor leaks on decoding failure, excess descriptors, and a truncated
    // ancillary buffer.
    for count in [1, 2, 8] {
        let (reader, writer) = UnixStream::pair().unwrap();
        reader
            .set_read_timeout(Some(std::time::Duration::from_secs(1)))
            .unwrap();
        let (sender, receiver) = UnixStream::pair().unwrap();
        let body = if count == 1 {
            vec![0xc1]
        } else {
            encode(&Request::Inspect { namespace_pid: 1 }).unwrap()
        };
        send_with_descriptors(&sender, &frame(&body), &[writer.as_fd(); 8][..count]);
        drop(sender);
        drop(writer);
        let result = receive::<Request>(&receiver);
        match count {
            1 => assert!(matches!(result, Err(Error::Decode(_)))),
            2 => assert_eq!(
                invalid(result),
                "control socket received excess descriptors"
            ),
            8 => assert_eq!(invalid(result), "control socket ancillary data truncated"),
            _ => unreachable!(),
        }
        assert_eq!((&reader).read(&mut [0]).unwrap(), 0);
    }
}

#[test]
fn connect_times_out_when_listener_backlog_is_full() {
    use rustix::net::{AddressFamily, SocketAddrUnix, SocketType, bind, listen, socket};
    use std::time::{Duration, Instant};
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("backlog.sock");
    let listener = socket(AddressFamily::UNIX, SocketType::STREAM, None).unwrap();
    bind(&listener, &SocketAddrUnix::new(&path).unwrap()).unwrap();
    listen(&listener, 0).unwrap();
    // Linux permits backlog + 1 pending connections.
    let _pending = cuinterpose_protocol::connect(&path, Duration::from_millis(100)).unwrap();
    let started = Instant::now();
    let error = cuinterpose_protocol::connect(&path, Duration::from_millis(100)).unwrap_err();
    assert_eq!(error.kind(), std::io::ErrorKind::WouldBlock);
    assert!(started.elapsed() >= Duration::from_millis(50));
    // Only a unit error, such as reading 100 ms as 100 s, can reach this bound. A loaded
    // host delays the wakeup by far less.
    assert!(started.elapsed() < Duration::from_secs(60));
}
