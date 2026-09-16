// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

fn main() {
    let root = "../../../../pagebroker/v1";
    let schema = format!("{root}/pagebroker.proto");
    println!("cargo:rerun-if-changed={schema}");
    prost_build::compile_protos(&[schema], &[root])
        .expect("generate the PageBroker allocation-session protocol");
}
