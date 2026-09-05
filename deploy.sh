#!/usr/bin/env bash
#
# Deploy do Papo em servidor Linux (Debian/Ubuntu, Fedora/RHEL, Arch), rodando como root:
#   1. Instala os pré-requisitos (git, curl, Go, Goose, Docker, Python 3 + venv).
#   2. Clona o repositório em um diretório temporário e compila o binário
#      (CGO_ENABLED=0, binário estático — mesma razão do build-and-run.sh: o
#      resolver DNS puro do Go evita spikes de ~5s e o binário fica portável).
#   3. Instala em $PREFIX: binário + worker Python de moderação + .env.
#   4. Sobe o PostgreSQL via docker compose e aplica as migrations com goose.
#   5. Cria o environment Python do worker de moderação (requirements.txt).
#   6. Registra o servidor como serviço systemd (papo.service).
#
# Reexecutar o script é seguro: .env existentes são preservados, migrations
# já aplicadas não rodam de novo e o binário é substituído pelo build mais
# recente (o script funciona também como atualização).
#
# Uso:
#   ./deploy.sh                # deploy completo com systemd
#   ./deploy.sh --no-systemd   # sem o serviço systemd (roda manualmente)
#
# Variáveis de ambiente (opcionais):
#   PAPO_PREFIX  diretório de instalação (padrão: /opt/papo)
#   PAPO_REPO    repositório git (padrão: https://github.com/Papo-Chat/papo-backend.git)
#   PAPO_BRANCH  branch para compilar (padrão: main)

set -euo pipefail

PREFIX="${PAPO_PREFIX:-/opt/papo}"
REPO_URL="${PAPO_REPO:-https://github.com/Papo-Chat/papo-backend.git}"
BRANCH="${PAPO_BRANCH:-main}"
INSTALL_SYSTEMD=true

# Versão mínima do Go exigida pelo backend/go.mod.
GO_MIN="1.26.5"

log()  { printf '\n==> %s\n' "$*"; }
die()  { printf '\nERRO: %s\n' "$*" >&2; exit 1; }

for arg in "$@"; do
    case "$arg" in
        --no-systemd) INSTALL_SYSTEMD=false ;;
        -h|--help)
            grep '^# ' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) die "opção desconhecida: $arg (use --help)" ;;
    esac
done

require_root() {
    [[ $EUID -eq 0 ]] || die "esta etapa requer root (execute o script como root)"
}

# ---------------------------------------------------------------------------
# Pré-requisitos
# ---------------------------------------------------------------------------

install_base_packages() {
    log "Verificando pacotes de base (git, curl, openssl, python3)..."
    local mgr
    if command -v apt-get >/dev/null 2>&1; then
        mgr=apt
    elif command -v dnf >/dev/null 2>&1; then
        mgr=dnf
    elif command -v pacman >/dev/null 2>&1; then
        mgr=pacman
    else
        die "gerenciador de pacotes não suportado (apt, dnf ou pacman)"
    fi

    local pkgs=()
    command -v git >/dev/null 2>&1 || pkgs+=(git)
    if ! command -v curl >/dev/null 2>&1; then
        case "$mgr" in
            apt) pkgs+=(curl ca-certificates) ;;
            *)   pkgs+=(curl) ;;
        esac
    fi
    command -v openssl >/dev/null 2>&1 || pkgs+=(openssl)
    if ! command -v python3 >/dev/null 2>&1; then
        case "$mgr" in
            pacman) pkgs+=(python) ;;
            *)      pkgs+=(python3) ;;
        esac
    elif ! python3 -c 'import ensurepip' 2>/dev/null; then
        # O ensurepip (necessário para criar o venv) é pacote separado no
        # Debian/Ubuntu (python3-venv) e no Fedora (python3-venv); no Arch o
        # pacote python já o inclui.
        pkgs+=(python3-venv)
    fi

    if (( ${#pkgs[@]} > 0 )); then
        require_root
        case "$mgr" in
            apt)
                apt-get update -qq
                DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${pkgs[@]}"
                ;;
            dnf)    dnf install -y "${pkgs[@]}" ;;
            pacman) pacman -S --noconfirm "${pkgs[@]}" ;;
        esac
    fi

    # O worker de moderação exige Python 3.10+ (requirements.txt pinado).
    python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 10) else 1)' \
        || die "Python 3.10+ é necessário (encontrado: $(python3 --version 2>&1))"
}

install_go() {
    log "Verificando Go (mínimo $GO_MIN)..."
    local have=""
    if command -v go >/dev/null 2>&1; then
        have="$(go env GOVERSION 2>/dev/null | sed 's/^go//' || true)"
    fi
    if [[ -n "$have" ]] && [[ "$(printf '%s\n%s\n' "$have" "$GO_MIN" | sort -V | head -n1)" == "$GO_MIN" ]]; then
        echo "    Go $have já instalado."
        return
    fi

    require_root
    local arch
    case "$(uname -m)" in
        x86_64)  arch=amd64 ;;
        aarch64) arch=arm64 ;;
        *) die "arquitetura não suportada: $(uname -m)" ;;
    esac

    local version url
    version="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -n1)"
    [[ -n "$version" ]] || die "falha ao descobrir a versão estável do Go"
    # dl.google.com serve o tarball E o .sha256 diretamente; go.dev/dl não
    # redireciona o .sha256 (devolve HTML, golang/go#41894).
    url="https://dl.google.com/go/${version}.linux-${arch}.tar.gz"

    echo "    Instalando Go $version ($arch)..."
    local tmp
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN
    curl -fsSL -o "$tmp/go.tgz" "$url"
    curl -fsSL -o "$tmp/go.tgz.sha256" "${url}.sha256"
    ( cd "$tmp" && echo "$(awk '{print $1}' go.tgz.sha256)  go.tgz" | sha256sum -c --quiet ) \
        || die "SHA-256 do tarball do Go não confere"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tmp/go.tgz"
    export PATH="/usr/local/go/bin:$PATH"
    go version
}

install_goose() {
    log "Verificando Goose..."
    if ! command -v goose >/dev/null 2>&1; then
        echo "    Instalando goose (go install)..."
        go install github.com/pressly/goose/v3/cmd/goose@latest
    fi
    export PATH="$HOME/go/bin:$PATH"
    command -v goose >/dev/null 2>&1 || die "falha ao instalar o goose"
    goose --version
}

install_docker() {
    log "Verificando Docker..."
    if ! command -v docker >/dev/null 2>&1; then
        require_root
        echo "    Instalando Docker (get.docker.com)..."
        curl -fsSL https://get.docker.com | sh
        systemctl enable --now docker 2>/dev/null || true
    fi
    docker compose version >/dev/null 2>&1 || die "docker compose indisponível"
    if ! docker info >/dev/null 2>&1; then
        require_root
        systemctl enable --now docker 2>/dev/null || true
    fi
    docker info >/dev/null 2>&1 || die "daemon do Docker não está respondendo"
}

# ---------------------------------------------------------------------------
# Build e instalação
# ---------------------------------------------------------------------------

clone_and_build() {
    log "Clonando $REPO_URL (branch: $BRANCH)..."
    SRC_DIR="$(mktemp -d)"
    git clone --quiet --depth 1 --branch "$BRANCH" "$REPO_URL" "$SRC_DIR/repo" \
        || die "falha ao clonar o repositório"
    echo "    commit: $(git -C "$SRC_DIR/repo" rev-parse --short HEAD)"

    log "Compilando o servidor (CGO_ENABLED=0)..."
    ( cd "$SRC_DIR/repo/backend" && CGO_ENABLED=0 go build -o papo-server ./cmd ) \
        || die "falha na compilação"
    echo "    binário: $SRC_DIR/repo/backend/papo-server"
}

install_files() {
    log "Instalando em $PREFIX..."
    install -d "$PREFIX/backend/moderation_worker" "$PREFIX/infra" "$PREFIX/migrations"

    # Sincroniza as migrations com o repositório: remove arquivos que saíram do
    # repositório (goose só aplica o que está no diretório; migrations já
    # aplicadas ficam registradas no banco e não dependem do arquivo).
    rm -f "$PREFIX/migrations/"*.sql

    install -m 0755 "$SRC_DIR/repo/backend/papo-server" "$PREFIX/backend/papo-server"
    install -m 0644 "$SRC_DIR/repo/backend/moderation_worker/worker.py" "$PREFIX/backend/moderation_worker/worker.py"
    install -m 0644 "$SRC_DIR/repo/backend/moderation_worker/requirements.txt" "$PREFIX/backend/moderation_worker/requirements.txt"
    install -m 0644 "$SRC_DIR/repo/backend/.env.sample" "$PREFIX/backend/.env.sample"
    install -m 0644 "$SRC_DIR/repo/infra/docker-compose.yml" "$PREFIX/infra/docker-compose.yml"
    install -m 0644 "$SRC_DIR/repo/infra/.env.sample" "$PREFIX/infra/.env.sample"
    install -m 0644 "$SRC_DIR/repo/migrations/"*.sql "$PREFIX/migrations/"

    # O servidor roda como usuário dedicado (sem privilégios).
    if [[ $EUID -eq 0 ]]; then
        if ! id -u papo >/dev/null 2>&1; then
            local nologin=""
            for p in /usr/sbin/nologin /sbin/nologin /usr/bin/nologin; do
                if [[ -x "$p" ]]; then nologin="$p"; break; fi
            done
            # Home em /nonexistent: com ProtectHome=true no unit, o home do
            # usuário fica inacessível ao serviço — não pode ser $PREFIX.
            useradd --system --no-create-home --shell "${nologin:-/usr/sbin/nologin}" --home-dir /nonexistent papo
        fi
        chown -R papo:papo "$PREFIX"
    else
        echo "    (sem root: pulando criação do usuário papo e chown)"
    fi
}

# ---------------------------------------------------------------------------
# Variáveis de ambiente
# ---------------------------------------------------------------------------

env_get() { # env_get <arquivo> <chave>
    grep -E "^$2=" "$1" 2>/dev/null | head -n1 | cut -d= -f2-
}

setup_envs() {
    log "Configurando variáveis de ambiente..."
    local db_user="papo" db_name="papo" db_port="5432" db_password=""

    if [[ -f "$PREFIX/infra/.env" ]]; then
        echo "    $PREFIX/infra/.env já existe — preservando."
        db_user="$(env_get "$PREFIX/infra/.env" DB_USER)"
        db_name="$(env_get "$PREFIX/infra/.env" DB_NAME)"
        db_port="$(env_get "$PREFIX/infra/.env" DB_PORT)"
        db_password="$(env_get "$PREFIX/infra/.env" DB_PASSWORD)"
        [[ -n "$db_password" ]] || die "DB_PASSWORD ausente em $PREFIX/infra/.env"
    else
        db_password="$(openssl rand -hex 16)"
        cat > "$PREFIX/infra/.env" <<EOF
# PostgreSQL (container docker compose)
DB_USER=$db_user
DB_PASSWORD=$db_password
DB_NAME=$db_name
DB_PORT=$db_port

GOOSE_DRIVER=postgres
GOOSE_DBSTRING=postgres://$db_user:$db_password@localhost:$db_port/$db_name
GOOSE_MIGRATION_DIR=$PREFIX/migrations
EOF
        echo "    $PREFIX/infra/.env criado (senha do banco gerada)."
    fi
    chmod 600 "$PREFIX/infra/.env"

    if [[ -f "$PREFIX/backend/.env" ]]; then
        echo "    $PREFIX/backend/.env já existe — preservando."
    else
        cp "$PREFIX/backend/.env.sample" "$PREFIX/backend/.env"
        local jwt_secret hmac_secret turn_secret
        jwt_secret="$(openssl rand -hex 32)"
        hmac_secret="$(openssl rand -hex 32)"
        turn_secret="$(openssl rand -hex 32)"
        sed -i \
            -e "s|^DATABASE_URL=.*|DATABASE_URL=postgres://$db_user:$db_password@localhost:$db_port/$db_name|" \
            -e "s|^JWT_SECRET=.*|JWT_SECRET=$jwt_secret|" \
            -e "s|^HMAC_SECRET=.*|HMAC_SECRET=$hmac_secret|" \
            -e "s|^TURN_SECRET=.*|TURN_SECRET=$turn_secret|" \
            -e "s|^MODERATION_WORKER_COMMAND=.*|MODERATION_WORKER_COMMAND=$PREFIX/backend/moderation_worker/.venv/bin/python3|" \
            "$PREFIX/backend/.env"
        echo "    $PREFIX/backend/.env criado (segredos gerados)."
    fi
    chmod 600 "$PREFIX/backend/.env"
    if [[ $EUID -eq 0 ]]; then
        chown papo:papo "$PREFIX/infra/.env" "$PREFIX/backend/.env"
    fi

    DB_USER="$db_user"
    DB_NAME="$db_name"
    DB_PORT="$db_port"
    DB_PASSWORD="$db_password"
}

# ---------------------------------------------------------------------------
# PostgreSQL + migrations
# ---------------------------------------------------------------------------

start_postgres() {
    log "Subindo o PostgreSQL (docker compose)..."
    ( cd "$PREFIX/infra" && docker compose up -d )

    local i
    for i in $(seq 1 60); do
        if docker exec papo_postgres pg_isready -U "$DB_USER" >/dev/null 2>&1; then
            echo "    PostgreSQL pronto."
            return
        fi
        sleep 1
    done
    die "PostgreSQL não ficou pronto em 60s (veja: docker logs papo_postgres)"
}

run_migrations() {
    log "Aplicando migrations (goose)..."
    goose -dir "$PREFIX/migrations" postgres \
        "postgres://$DB_USER:$DB_PASSWORD@localhost:$DB_PORT/$DB_NAME?sslmode=disable" up
    echo "    Migrations aplicadas."
}

# ---------------------------------------------------------------------------
# Worker Python (moderação)
# ---------------------------------------------------------------------------

setup_python_env() {
    log "Configurando o environment Python do worker de moderação..."
    local venv="$PREFIX/backend/moderation_worker/.venv"
    local reqs="$PREFIX/backend/moderation_worker/requirements.txt"

    # Reexecução/atualização: só recria se o venv não existir, estiver
    # quebrado ou o requirements.txt tiver mudado.
    if [[ -x "$venv/bin/python" ]] && "$venv/bin/python" -c 'import sys' 2>/dev/null \
        && cmp -s "$reqs" "$venv/.requirements.txt" 2>/dev/null; then
        echo "    $venv já está em dia."
    else
        python3 -m venv "$venv"
        "$venv/bin/pip" install --quiet --disable-pip-version-check -r "$reqs"
        cp "$reqs" "$venv/.requirements.txt"
    fi
    if [[ $EUID -eq 0 ]]; then
        chown -R papo:papo "$venv"
    fi
    echo "    $venv pronto ($("$venv/bin/python" --version 2>&1))."
}

# ---------------------------------------------------------------------------
# systemd
# ---------------------------------------------------------------------------

install_systemd() {
    log "Registrando o serviço systemd (papo.service)..."
    local state
    state="$(systemctl is-system-running 2>/dev/null || true)"
    case "$state" in
        # starting/initializing: boot em andamento — o systemd está lá.
        running|degraded|starting|initializing) ;;
        *) die "systemd não está disponível neste sistema (use --no-systemd)" ;;
    esac

    if systemctl is-active --quiet papo 2>/dev/null; then
        systemctl stop papo
    fi

    cat > /etc/systemd/system/papo.service <<EOF
[Unit]
Description=Papo chat server
After=network-online.target docker.service
Wants=network-online.target

[Service]
User=papo
Group=papo
WorkingDirectory=$PREFIX/backend
ExecStart=$PREFIX/backend/papo-server
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=full

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable --now papo

    local server_port i
    server_port="$(env_get "$PREFIX/backend/.env" SERVER_PORT)"
    server_port="${server_port:-8080}"
    for i in $(seq 1 30); do
        if curl -fsS "http://localhost:${server_port}/health" >/dev/null 2>&1; then
            echo "    Serviço no ar (health OK em http://localhost:${server_port}/health)."
            return
        fi
        sleep 1
    done
    die "servidor não passou no health check em 30s (veja: journalctl -u papo --no-pager -n 50)"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    log "Deploy do Papo em $PREFIX"
    trap 'rm -rf "${SRC_DIR:-}"' EXIT

    install_base_packages
    install_go
    install_goose
    install_docker
    clone_and_build
    install_files
    setup_envs
    start_postgres
    run_migrations
    setup_python_env
    if $INSTALL_SYSTEMD; then
        install_systemd
    else
        log "systemd pulado (--no-systemd). Para rodar manualmente:"
        echo "    cd $PREFIX/backend && ./papo-server"
    fi

    log "Deploy concluído."
    cat <<EOF

  Instalação:  $PREFIX
    backend/            binário + worker de moderação + .env
    infra/              docker-compose.yml + .env (credenciais do banco)
    migrations/         migrations goose
  Ports:       8080/tcp (HTTP + WebSocket), 50000/udp (voz), 5432 (PostgreSQL, somente localhost)
  Serviço:     systemctl status papo   (se systemd habilitado)
  Logs:        journalctl -u papo -f
  Config:      edite $PREFIX/backend/.env e reinicie (systemctl restart papo)
  Moderação:   defina MODERATION_ENABLED=true no .env e reinicie
EOF
}

main
