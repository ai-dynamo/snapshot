FROM golang:1.26-alpine AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o manager ./cmd/manager

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/manager /manager
ENTRYPOINT ["/manager"]
