# syntax=docker/dockerfile:1
# check=skip=InvalidDefaultArgInFrom

# Go is supplied by Mise through the release workflow or local container task.
ARG GO_VERSION

# ---- Go build -------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

RUN apk add --no-cache upx
WORKDIR /workspace

# Cache module downloads before copying source.
COPY go.mod go.sum ./
RUN go mod download
ARG GO_LICENSES_VERSION
RUN go install github.com/google/go-licenses/v2@${GO_LICENSES_VERSION}

COPY cmd/ cmd/
COPY internal/ internal/
COPY plugin/ plugin/

# go-sqlite3-wasm/v6 is MIT-0, which google/licenseclassifier does not currently recognize: https://github.com/google/licenseclassifier/issues/75
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go-licenses save ./cmd/stemma --save_path third_party_licenses --ignore github.com/woodleighschool/stemma,github.com/ncruces/go-sqlite3-wasm/v6 --force

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION#v} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o stemma ./cmd/stemma
RUN upx --best --lzma stemma

# ---- Runtime --------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY LICENSE /LICENSE
COPY --from=builder /workspace/third_party_licenses /third_party_licenses
COPY --from=builder /usr/local/go/LICENSE /third_party_licenses/go/LICENSE
COPY --from=builder /workspace/stemma /stemma
USER 65532:65532
ENTRYPOINT ["/stemma"]
