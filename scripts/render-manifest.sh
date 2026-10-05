#!/usr/bin/env bash
# render-manifest.sh IMAGE@DIGEST VERSION: writes the manifest the release
# publishes, pinned to the image just built, into dist/package/. The
# manifest in the repository carries a zero digest; this is the only place
# a real one goes in, and it refuses a version that does not match the tag.
set -euo pipefail
ref="$1"; version="$2"
[[ "$ref" =~ ^[a-z0-9./-]+@sha256:[0-9a-f]{64}$ ]] || { echo "not an image pinned by digest: $ref" >&2; exit 1; }
want=$(sed -n 's/^version: //p' mcplocker-provider.yaml)
[[ "$want" == "$version" ]] || { echo "the manifest says version $want, the tag says $version" >&2; exit 1; }
mkdir -p dist/package
sed "s|^  image: .*$|  image: $ref|" mcplocker-provider.yaml > dist/package/mcplocker-provider.yaml
cp icon.svg dist/package/
grep -q "^  image: $ref$" dist/package/mcplocker-provider.yaml || { echo "the image line was not replaced" >&2; exit 1; }
