FROM golang:1.26-alpine AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o agent ./cmd/agent && go build -o nsrestore ./cmd/nsrestore

FROM ubuntu:22.04
RUN apt-get update && apt-get install -y criu && rm -rf /var/lib/apt/lists/*
COPY --from=builder /workspace/agent /agent
COPY --from=builder /workspace/nsrestore /nsrestore
ENTRYPOINT ["/agent"]
