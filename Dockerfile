# syntax=docker/dockerfile:1.7

FROM node:24-alpine AS dashboard-build

WORKDIR /src

COPY web/dashboard/package.json web/dashboard/package-lock.json ./web/dashboard/
RUN npm --prefix web/dashboard ci

COPY web/dashboard ./web/dashboard
RUN npm --prefix web/dashboard run build

FROM --platform=$BUILDPLATFORM golang:1.26.7-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=dashboard-build /src/internal/dashboard/dist ./internal/dashboard/dist
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
	go build -trimpath -ldflags="-s -w -X asterferry/internal/buildinfo.Version=${VERSION} -X asterferry/internal/buildinfo.Commit=${COMMIT} -X asterferry/internal/buildinfo.BuildDate=${BUILD_DATE}" -o /out/asterferry ./cmd/asterferry

FROM alpine:3.20

RUN apk add --no-cache ca-certificates wget \
    && addgroup -S -g 10001 asterferry \
    && adduser -S -D -H -u 10001 -G asterferry asterferry \
    && mkdir -p /etc/asterferry/secrets \
    && chown -R asterferry:asterferry /etc/asterferry

COPY --from=build /out/asterferry /usr/local/bin/asterferry

USER 10001:10001
WORKDIR /etc/asterferry

EXPOSE 4433/udp 9090/tcp 9091/tcp
ENTRYPOINT ["/usr/local/bin/asterferry"]
