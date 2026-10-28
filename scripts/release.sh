#!/usr/bin/env bash
# 本地打包脚本，产出与 GitHub Actions 完全一致的发布产物：
#   dist/clashgo-<os>-<arch>.tar.gz
#   dist/checksums.txt
#
# 用法： scripts/release.sh [版本号]      # 版本号缺省取 git describe
# 之后把 dist/ 里的文件传到任意 Releases / 静态目录即可让 `clashgo upgrade --self` 用上。
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
DIST=dist

echo "▸ 版本：$VERSION"
rm -rf "$DIST"
mkdir -p "$DIST"

for os in linux darwin windows; do
    for arch in amd64 arm64; do
        bin=clashgo
        [ "$os" = windows ] && bin=clashgo.exe
        echo "▸ 构建 $os/$arch"
        CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
            go build -trimpath \
            -ldflags "-s -w -X clashgo/cmd.Version=$VERSION" \
            -o "$DIST/$bin" .
        # 产物名必须与 internal/selfupdate.AssetName() 一致
        ( cd "$DIST" && tar czf "clashgo-$os-$arch.tar.gz" "$bin" && rm -f "$bin" )
    done
done

( cd "$DIST" && sha256sum ./*.tar.gz | sed 's|  \./|  |' > checksums.txt )
echo
cat "$DIST/checksums.txt"
