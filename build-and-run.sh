#!/usr/bin/env bash
#
# Build e run do backend Papo SEM cgo (binário estático puro Go).
#
# Por que CGO_ENABLED=0:
#   O resolver DNS puro do Go consulta todos os nameservers em paralelo e
#   pega a 1ª resposta. O resolver cgo/glibc (usado quando CGO está habilitado)
#   consulta os nameservers sequencialmente e espera o timeout do resolv.conf
#   (~5s) se o 1º nameserver perder o pacote UDP. Isso causava spikes de ~5s na
#   resolução DNS, estourando o timeout de 2s do robots.txt e derrubando link
#   previews (YouTube/GitHub). Com CGO_ENABLED=0 o problema some e o binário
#   fica estático/portável (sem dependências de libc/glibc).
#
# Uso:
#   ./build-and-run.sh          # build + run
#   ./build-and-run.sh build    # apenas build
#   ./build-and-run.sh run      # apenas run (usa o binário já buildado)

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKEND_DIR="$ROOT_DIR/backend"
BINARY="$ROOT_DIR/papo-server"

build() {
    echo "==> Build (CGO_ENABLED=0, binário estático puro Go)"
    cd "$BACKEND_DIR"
    CGO_ENABLED=0 go build -o "$BINARY" ./cmd
    echo "==> Binário: $BINARY"
}

run() {
    if [[ ! -x "$BINARY" ]]; then
        echo "Binário não encontrado ($BINARY). Rode: $0 build" >&2
        exit 1
    fi
    # O config carrega o .env via godotenv a partir do CWD → roda em backend/
    echo "==> Rodando servidor (CWD=$BACKEND_DIR para carregar .env)"
    cd "$BACKEND_DIR"
    exec "$BINARY"
}

case "${1:-}" in
    build) build ;;
    run)   run ;;
    "")    build && run ;;
    *)
        echo "Uso: $0 [build|run]" >&2
        exit 1
        ;;
esac
