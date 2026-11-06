FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/raftlogd ./cmd/raftlogd \
 && CGO_ENABLED=0 go build -o /out/raftlogctl ./cmd/raftlogctl

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/raftlogd /out/raftlogctl /usr/local/bin/
VOLUME /data
ENTRYPOINT ["/usr/local/bin/raftlogd"]
