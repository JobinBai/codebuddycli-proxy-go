#!/usr/bin/env sh
set -eu

version="${1:-dev}"
app="codebuddycli-proxy"
rm -rf dist
mkdir -p dist

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os="${target%/*}"
  arch="${target#*/}"
  stage="dist/${app}_${version}_${os}_${arch}"
  mkdir -p "$stage"
  ext=""
  [ "$os" = windows ] && ext=".exe"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=${version}" -o "$stage/$app$ext" "./cmd/$app"
  if [ "$os" = windows ]; then
    (cd "$stage" && zip -q "../${app}_${version}_${os}_${arch}.zip" "$app$ext")
  else
    tar -C "$stage" -czf "dist/${app}_${version}_${os}_${arch}.tar.gz" "$app"
  fi
  rm -rf "$stage"
done
