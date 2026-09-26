# One build, two images (docs/QUESTIONS.md D10):
#   docker build --target sidecar      -t sidecar:dev .
#   docker build --target controlplane -t sidecar-controlplane:dev .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/sidecar ./cmd/sidecar \
 && CGO_ENABLED=0 go build -trimpath -o /out/controlplane ./cmd/controlplane

FROM gcr.io/distroless/static:nonroot AS controlplane
COPY --from=build /out/controlplane /
USER nonroot
ENTRYPOINT ["/controlplane"]

# Last stage, so a plain `docker build .` still produces the sidecar.
FROM gcr.io/distroless/static:nonroot AS sidecar
COPY --from=build /out/sidecar /
USER nonroot
ENTRYPOINT ["/sidecar"]
