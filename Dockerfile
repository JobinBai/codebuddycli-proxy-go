FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/codebuddycli-proxy ./cmd/codebuddycli-proxy

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/codebuddycli-proxy /codebuddycli-proxy
USER 65532:65532
ENV HOST=0.0.0.0 PORT=8787
EXPOSE 8787
ENTRYPOINT ["/codebuddycli-proxy"]
