FROM golang:1.19 as builder

WORKDIR /workspace
COPY go.mod go.sum* ./
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/controller/ internal/controller/

RUN CGO_ENABLED=0 GOOS=linux go build -a -o manager cmd/main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
