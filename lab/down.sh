#!/usr/bin/env bash
# Removes everything the lab made: the k3s container, the pool, the Go
# caches and the images. Nothing else is touched.
set -uo pipefail
source "$(dirname "$0")/lib.sh"
docker rm -f "$LAB" >/dev/null 2>&1
docker ps -aq --filter "label=$LABEL" | xargs -r docker rm -f >/dev/null
docker volume ls -q --filter "label=$LABEL" | xargs -r docker volume rm >/dev/null
docker volume rm fern-lab-gomod fern-lab-gocache fern-lab-lintcache >/dev/null 2>&1
docker image ls -q --filter "label=$LABEL" | xargs -r docker image rm -f >/dev/null 2>&1
echo "lab is down"
