# syntax=docker/dockerfile:1
# Image de Cercle : binaire Go statique dans scratch.
# La compilation est croisée (GOARCH) sur la plateforme de build : aucune
# émulation n'est nécessaire, quelle que soit l'architecture visée.
#
#   docker build --network host -f deploy/Dockerfile -t cercle .
#   docker buildx build --network host --platform linux/amd64,linux/arm64,linux/arm/v7 -f deploy/Dockerfile .

FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev COMMIT=inconnu DATE=inconnue
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w \
        -X github.com/DB-CL/cercle/internal/buildinfo.Version=$VERSION \
        -X github.com/DB-CL/cercle/internal/buildinfo.Commit=$COMMIT \
        -X github.com/DB-CL/cercle/internal/buildinfo.Date=$DATE" \
      -o /out/cercle ./cmd/cercle \
 && mkdir -p /out/root/data /out/root/share /out/root/downloads /out/root/tmp

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/root/ /
COPY --from=build /out/cercle /cercle
USER 65532:65532
EXPOSE 51820/udp 8080/tcp
VOLUME ["/data", "/downloads"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/cercle", "healthcheck"]
ENTRYPOINT ["/cercle"]
CMD ["serve"]
