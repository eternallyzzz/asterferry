# The image uses only directives supported by Docker's built-in frontend. Keep
# the parser local so release builds do not depend on a second registry fetch.
FROM docker.io/library/node:24-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43 AS dashboard-build

WORKDIR /src

COPY web/dashboard/package.json web/dashboard/package-lock.json ./web/dashboard/
# npm 12 treats lockfile tarballs from alternate registries as remote packages.
# Keep the release build on the canonical registry while accepting a mirrored lockfile.
RUN npm install --global npm@12.0.2 --registry=https://registry.npmjs.org \
	&& npm --prefix web/dashboard ci --audit=false --registry=https://registry.npmjs.org --replace-registry-host=always

COPY web/dashboard ./web/dashboard
RUN npm --prefix web/dashboard run build

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.7-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build

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
	go build -tags=dashboard_assets -trimpath -ldflags="-s -w -X asterferry/internal/buildinfo.Version=${VERSION} -X asterferry/internal/buildinfo.Commit=${COMMIT} -X asterferry/internal/buildinfo.BuildDate=${BUILD_DATE}" -o /out/asterferry ./cmd/asterferry

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/asterferry /usr/local/bin/asterferry

USER 10001:10001
WORKDIR /etc/asterferry

EXPOSE 8443/tcp 9443/tcp 9090/tcp 4433/udp
ENTRYPOINT ["/usr/local/bin/asterferry"]
