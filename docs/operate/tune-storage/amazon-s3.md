<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Amazon S3

Use this page to tune S3 transfers for Snapshot restores. It covers where to
place the bucket, how to choose an instance with enough network bandwidth, and
how to set up parallel reads. These are starting recommendations, and there are
no published Snapshot benchmark results for them.

To connect Snapshot to the bucket first, including the endpoint and credentials,
see [Other Backends](../configure-storage.md#other-backends). For other storage
options, see [Tune Storage](README.md).

## Setup

### Keep the Bucket Near the Restore Nodes

Place the bucket and EC2 restore nodes in the same AWS Region. Set
`storage.s3.region` to that Region and use its S3 endpoint. This reduces
network latency and data-transfer costs. See [AWS's S3 performance guidance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance-guidelines.html).

Use an [S3 gateway endpoint](https://docs.aws.amazon.com/vpc/latest/privatelink/vpc-endpoints-s3.html)
for the restore nodes' subnet route tables. This routes S3 traffic without a
NAT device or internet gateway.

## Tuning

### Check the Instance's Sustained Bandwidth

Choose the instance size by its sustained network bandwidth as well as its
GPU capacity. Larger sizes often provide more bandwidth within an instance
family. An advertised "up to" rate can depend on burst credits; check the
baseline rate for a long restore or several concurrent restores.

Inspect the network-card specifications for your instance type:

```bash
aws ec2 describe-instance-types --region "<region>" \
  --instance-types "<instance-type>" \
  --query 'InstanceTypes[0].NetworkInfo.NetworkCards[].{Card:NetworkCardIndex,BaselineGbps:BaselineBandwidthInGbps,PeakGbps:PeakBandwidthInGbps}'
```

Check [EC2 network bandwidth](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-network-bandwidth.html)
for limits on the actual route. A large instance's aggregate network bandwidth
does not mean a single S3 connection can use all of it.

### Use Parallel Reads

S3 needs concurrent requests to use high network bandwidth. Configure
`pageBroker.transferEngine.modelStreamer` using the
[reader settings reference](../../../agent/pagebroker/MODEL_STREAMER.md#input-contract).

Start with the defaults. If network use is low during S3 reads, increase
connections and concurrency while watching CPU and memory use. Restart
PageBroker after changing reader settings; they are read at startup.

Size `pageBroker.staging.sizeLimit` for the staged snapshot. Reader buffers
are a separate memory cost: `maxInflightMiB` applies per native client and
does not cap total staging or SDK memory. Account for reader and SDK memory
in PageBroker's memory budget too. See
[staging memory](../configure-storage.md#pagebroker-staging) for sizing the staged snapshot.

## Verify

Measure parallel S3 downloads from the same node and route used by PageBroker.
Also record bytes, transfer time, instance type, Region, and reader settings.
For downloads to a file, measure the destination disk separately with fio.

After you change a setting on this page, check the result with a real restore. Follow [Repeat the Real Restore](../../troubleshooting/slow-restores.md#repeat-the-real-restore) and compare the restore time with the time before the change.
