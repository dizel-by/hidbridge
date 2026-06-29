#!/usr/bin/env bash
# Run idf.py inside the official Espressif Docker image — no local IDF install.
#
# Examples:
#   ./idf.sh set-target esp32s3
#   ./idf.sh build
#   ./idf.sh -p /dev/ttyUSB0 flash         # needs the ESP connected
#   ./idf.sh -p /dev/ttyUSB0 monitor
#
# Override the IDF version with IDF_TAG, e.g. IDF_TAG=release-v5.4 ./idf.sh build
set -euo pipefail

IDF_TAG="${IDF_TAG:-release-v5.3}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Pass through serial devices if present so flash/monitor work.
devices=()
for d in /dev/ttyUSB0 /dev/ttyACM0; do
  [ -e "$d" ] && devices+=(--device "$d")
done

exec docker run --rm -it \
  -v "$DIR":/project -w /project \
  -u "$(id -u):$(id -g)" -e HOME=/tmp \
  "${devices[@]}" \
  "espressif/idf:${IDF_TAG}" \
  idf.py "$@"
