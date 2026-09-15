#!/bin/sh
set -eu

output=${1:-build/rclone-drime}
patches=drime-relative-path,pagination-fail-fast,presigned-upload-batches,verify-uploads
version=$(git describe --tags --always --dirty)

mkdir -p "$(dirname "$output")"
go build -trimpath \
	-ldflags "-s -X github.com/rclone/rclone/fs.Version=$version -X github.com/rclone/rclone/cmd.PbsRsyncPatches=$patches" \
	-o "$output" .
