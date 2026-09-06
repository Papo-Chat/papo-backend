#!/usr/bin/env bash
#
# Stress test completo do backend Papo:
#   1. cria um banco novo (papo_stress) e aplica as migrations;
#   2. sobe o backend nele com rate limits altos e previews/thumbnails off;
#   3. roda a tool de stress (seeding em fatias + medição após cada fatia).
#
# Uso:
#   ./stress.sh           # escala full (1000 users, 500k msgs, ~3GB em disco)
#   ./stress.sh small     # escala pequena (30 users, 3k msgs) para validar o setup
#
# Pré-requisitos:
#   - Postgres rodando: docker compose up -d postgres (container papo_postgres)
#   - goose no PATH: go install github.com/pressly/goose/v3/cmd/goose@latest
#
# O banco papo_stress fica no lugar após o run, para inspeção ou
# -measure-only. Para removê-lo:
#   docker exec papo_postgres dropdb -U papo papo_stress

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKEND_DIR="$ROOT_DIR/backend"
DB_NAME="papo_stress"
DB_URL="postgres://papo:papo123@localhost:5432/${DB_NAME}?sslmode=disable"
PORT=8090
SERVER_LOG="$BACKEND_DIR/stress-server.log"

# --- escala ---
USERS=1000; MESSAGES=500000; CHANNELS=40; EMOJIS=500; CHUNKS=10
CONCURRENCY=16; USER_CONC=8; MEASURE_CONC=20; REQUESTS=2000
case "${1:-full}" in
    small)
        USERS=30; MESSAGES=3000; CHANNELS=5; EMOJIS=15; CHUNKS=3
        CONCURRENCY=8; USER_CONC=4; MEASURE_CONC=8; REQUESTS=300
        ;;
    full) : ;;
    *)
        echo "Uso: $0 [full|small]" >&2
        exit 1
        ;;
esac

REPORT_DIR="$BACKEND_DIR/tmp/stress-report"

# --- pré-checks ---
if ! command -v goose >/dev/null 2>&1; then
    echo "goose não encontrado no PATH. Instale: go install github.com/pressly/goose/v3/cmd/goose@latest" >&2
    exit 1
fi
if ! docker exec papo_postgres psql -U papo -c "SELECT 1" >/dev/null 2>&1; then
    echo "Postgres não acessível (container papo_postgres). Suba com: docker compose up -d postgres" >&2
    exit 1
fi

# --- 1. banco novo ---
echo "==> Banco $DB_NAME: drop + create + migrations"
if ! docker exec papo_postgres dropdb -U papo --if-exists "$DB_NAME"; then
    echo "Falha ao dropar $DB_NAME — há conexões ativas? Mate o papo-server antigo e tente de novo." >&2
    exit 1
fi
docker exec papo_postgres createdb -U papo "$DB_NAME"
goose postgres "$DB_URL" -dir "$ROOT_DIR/migrations" up

# --- 2. backend com rate limit alto ---
echo "==> Build do backend (CGO_ENABLED=0)"
cd "$BACKEND_DIR"
CGO_ENABLED=0 go build -o "$ROOT_DIR/papo-server" ./cmd

echo "==> Servidor na porta $PORT (logs em $SERVER_LOG)"
# CWD=backend/ para o godotenv carregar o .env; as variáveis exportadas
# abaixo têm prioridade sobre o .env.
export DATABASE_URL="$DB_URL"
export SERVER_PORT="$PORT"
export RATE_LIMIT=1000 RATE_BURST=2000
export AUTH_RATE_LIMIT=200 AUTH_RATE_BURST=400
export LINK_PREVIEW_ENABLED=false THUMBNAIL_ENABLED=false MODERATION_ENABLED=false
"$ROOT_DIR/papo-server" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
trap 'kill "$SERVER_PID" 2>/dev/null || true' EXIT

echo "==> Aguardando /health"
for _ in $(seq 1 60); do
    if curl -sf "http://localhost:$PORT/health" >/dev/null; then
        break
    fi
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
        echo "Servidor morreu ao subir. Últimas linhas de $SERVER_LOG:" >&2
        tail -20 "$SERVER_LOG" >&2
        exit 1
    fi
    sleep 0.5
done
if ! curl -sf "http://localhost:$PORT/health" >/dev/null; then
    echo "Servidor não respondeu a /health em 30s. Veja $SERVER_LOG" >&2
    exit 1
fi

# --- 3. stress ---
echo "==> Stress: $USERS users, $MESSAGES msgs, $CHANNELS canais, $CHUNKS chunks, $REQUESTS req/fase"
go run ./cmd/stress -url "http://localhost:$PORT" \
    -users "$USERS" -messages "$MESSAGES" -channels "$CHANNELS" \
    -emojis "$EMOJIS" -chunks "$CHUNKS" \
    -concurrency "$CONCURRENCY" -user-concurrency "$USER_CONC" \
    -measure-concurrency "$MEASURE_CONC" -requests "$REQUESTS" \
    -report-dir "$REPORT_DIR"

echo
echo "==> Concluído."
echo "    Relatório: $REPORT_DIR (report.json, report.csv, chart.svg)"
echo "    Banco $DB_NAME mantido; para remover: docker exec papo_postgres dropdb -U papo $DB_NAME"
