#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

function main {
    build_docset latest
    build_docset 8.0.0
    build_docset 7.6.0
    build_docset 7.5.0
    build_docset 7.4.0
    build_docset 7.0.0
    build_docset 6.5.0
}

function build_docset {
    version="$1"
    output_dir="$repo_root/docset_versions/$version"
    rm -rf $output_dir
    mkdir -p $output_dir/bazel.docset

    pushd "versions/$version"

    go run ../../dashing.go build \
        --config "$repo_root/config.yaml" \
        --output "$output_dir/bazel.docset" \
        --version "$version"

    tar --exclude='.DS_Store' -cvzf $output_dir/bazel.tgz $output_dir/bazel.docset

    version_dir="$version"
    if [[ "$version" == "latest" ]]; then
        version_dir=""
    fi

    docset_dest="$repo_root/../Dash-User-Contributions/docsets/bazel/versions/$version_dir"
    mkdir -p "$docset_dest"

    cp "$output_dir/bazel.tgz" "$docset_dest/bazel.tgz"

    popd
}

main
