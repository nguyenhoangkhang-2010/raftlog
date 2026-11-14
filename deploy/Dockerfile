FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
# One image holds every program that the Compose file runs; each service picks its own with `entrypoint`.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/ \
      ./cmd/raftlogd ./cmd/raftlogctl ./cmd/orderservice ./cmd/raftlogconsole \
 && mkdir /data

# nonroot is user 65532. The directory is made here, owned by that user, so that a new volume mounted on /data
# starts with the right owner.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY --from=build --chown=65532:65532 /data /data
# The sample menu and promotions, for trying the kitchen service (invented, not a real venue).
COPY --from=build /src/examples/kitchen /examples/kitchen
VOLUME /data
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/raftlogd"]
