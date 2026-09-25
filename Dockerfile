# One image, both binaries: /sidecar and /controlplane.
#   docker build -t sidecar:dev .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/sidecar ./cmd/sidecar \
 && CGO_ENABLED=0 go build -trimpath -o /out/controlplane ./cmd/controlplane

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/sidecar /out/controlplane /
USER nonroot
