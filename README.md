[![License: AGPL v3](https://img.shields.io/badge/License-AGPLv3-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)
[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go)](https://go.dev)
[![Release](https://img.shields.io/github/v/release/Papo-Chat/papo-backend)](https://github.com/Papo-Chat/papo-backend/releases)
[![codecov](https://codecov.io/github/Papo-Chat/papo-backend/graph/badge.svg?token=XVOL7MZPNS)](https://codecov.io/github/Papo-Chat/papo-backend)

# Papo — Backend

Backend do Papo: Um chat self-hosted, inspirado no Discord dos primeiros anos: simples, leve e sem microserviços. Um binário Go, um Postgres, um servidor por instância, onde um Administrador ou Dev. Júnior consegue facilmente entender e manter em operação.

## Stack

- **Echo** — HTTP 
- **PGX** — PostgreSQL
- **Gorilla WebSocket** — realtime
- **JWT** + **Bcrypt** — autenticação
- **Goose** — migrations
- **Logrus** — logging
- **Go Puro** - Sem cgo ou microserviços, binário leve e focado em rodar com o mínimo de recursos possível.

## Roadmap (V1)

- [x] Backend MVP completo (endpoints, websocket, com segurança, testes e performance)
- [x] Event para troca de avatar/nickname
- [x] CORS configurável via vars. (util para rede local/teste)
- [x] Processamento Thumbnail no backend (com oEmbed, Opengraph, segurança reforçada)
- [x] Build e run script sem cgo, binário menor
- [x] Implementar evento user_join quando um novo usuário se registrar (para o frontend conseguir adicionar a lista de usuário).
- [x] Status Ausente/Ocupado (novo field)
- [x] Seek (HTTP206) para attachment de vídeos 
- [x] Adições no User Profile (Descrição, banner), GET /media/:sha_hash e last_read_message
- [x] Batch request para user profiles (POST /users/profile_batch com body com ids dos usuários), retorna o mesmo que profile mas array (máximo 50 usuários)
- [x] Fixar código em single server
- [x] Auditoria/Logs para Admins 
- [x] Crons GC Attachments orfãos/tabela quebrada e limpeza de logs.
- [x] Pin message /POST 
- [x] React Mensagens
- [x] Notificações
- [x] Fortificar Auth (Refresh Token)
- [ ] Suporte WebRTC (Audio, Video, Transmissão)
- [ ] Detecção e moderação Automática (Contra conteúdo sensível)
- [ ] Suporte Cloudflare (env CLOUDFLARE_PROXY que = true, ao invés de puxar o IP do context pra coisas tipo ratelimit), que valida se o ip é da cloudflare e se for, puxa o IP do header CF- correspondente.

### V2:

- [ ] Hashed Resync para conexão instável
- [ ] 2P-Auth (Authenticator)
- [ ] Direct Messages / Block User
- [ ] Bot API
- [ ] Mention Roles
- [ ] Mais User Permissions
- [ ] Mais User Settings
- [ ] Threads
- [ ] Favoritos (GIF, Emoji)
- [ ] Events Scheduling
- [ ] Estudo viabilidade WebTransport 
- [ ] Suporte NoSQL (intercambeavel nas settings) ou particionamento postgresql

## Arquitetura

Monólito modular. REST e WebSocket compartilham a mesma camada de `services` — os handlers só cuidam de transporte, validação de entrada e resposta.

```
REST ──────────┐
               ▼
          Services
               │
               ▼
          PostgreSQL
               │
               ▼
       WebSocket Hub
               │
               ▼
     Clientes conectados
```

```
backend/
├── cmd/                # entry point
├── internal/
│   ├── handlers/        # HTTP e WebSocket
│   ├── models/           # structs / DTOs
│   ├── services/          # regras de negócio
│   ├── websocket/          # hub, client, eventos
│   ├── storage/             # conexão com o banco
│   ├── middleware/           # auth, rate limit, cors, permissões
│   ├── utils/                 # helpers
│   └── config/                 # variáveis de ambiente
├── attachments/            # arquivos enviados (armazenados por SHA-256)
├── go.mod
└── go.sum
```

## Modelo

- **1 backend = 1 servidor.** Sem multi-tenant, sem federação — cada comunidade roda sua própria instância.
- **Servidor público ou com senha.** Acesso público é só conectar na URL; servidor privado exige senha, validada contra usuário já autenticado (sessão via cookie `HttpOnly` + JWT).
- **Servidor autoritativo.** O cliente nunca é fonte de verdade — permissão, presença, validação de arquivo e estado de canal são sempre decididos e persistidos no backend.
- **Attachments endereçados por conteúdo** (SHA-256), com deduplicação automática.
- **Thumbnail e Link Preview** Processamento de thumbnails muito robusto com etiqueta para robots.txt (oEmbed, OpenGraph, Youtube etc.)
- **Roles** Sistema de roles simplificado com permissões para acessar canal, moderação e administração.
- **Manutenção Automática** Crons autônomas que visam limpar e corrigir estados inválidos do servidor.
- **Logging** Logs de interações dos usuários que visa cumprir LGPD e GDRP, sem log explicito de IP e com limpeza frequente. 

## Pré-requisitos:
* Go 1.21+, Docker (para o Postgres), [Goose](https://github.com/pressly/goose).
* ~3GB RAM para processamento de thumbnails e links.

Versão leve: 
* 256MB-1GB RAM com processamento de thumbnails e links desativados:
```
.env:
     THUMBNAIL_ENABLED=false
```

## Rodando localmente ou em VPS Simples

```bash
# banco
cd infra && docker-compose up -d
```

```bash
# migrations
# necessário configurar .env do goose na pasta infra
goose up
```

```bash
# backend
chmod +x build-and-run.sh
./build-and-run.sh
```

Backend sobe em `http://localhost:8080`, WebSocket em `ws://localhost:8080/ws`.

### Variáveis de ambiente

```env
SERVER_PORT=8080

# MUDE O USUARIO E SENHA DO DATABASE EM PRODUÇÃO
DATABASE_URL=postgres://papo:papo123@localhost:5432/papo

# Use segredos aleatórios fortes em produção
# Usado para senhas e auth, OBRIGATÓRIA no mínimo 256bits
JWT_SECRET=troque_por_um_segredo_aleatorio

# Usado para que o endpoint de midia não sirva hash padrão
# USE CHAVES DIFERENTES
HMAC_SECRET=segredo_de_midia_unguessable

# Url usada para erros RFC 7807, pode ser deixada em branco sem problema
BASE_URL=https://papo.com/

# Variáveis de usuário
MAX_USERNAME_LENGTH=16
MAX_PASSWORD_LENGTH=64

# Limites de autenticação
AUTH_RATE_LIMIT=5
AUTH_RATE_BURST=10

# Limite geral
RATE_LIMIT=20
RATE_BURST=40

# Origens permitidas, separadas por vírgula
CORS_ORIGINS=http://localhost:5173,https://localhost:5173

# Cookies gerados aceitam frontend hosteado em endereço diferente do backend com false, nem um pouco recomendado por questões de segurança mas necessário em ambientes caseiros e de testes.
SAME_ORIGIN=true

# Desative para reduzir consumo de RAM, mas processamento de imagens será desativado
THUMBNAIL_ENABLED=true
```

## API

Esquema completo em [`openapi.yml`](./openapi.yml).

| Recurso | Endpoints principais |
|---|---|
| Auth | `/auth/register`, `/auth/login`, `/auth/login_server`, `/auth/whoami`, `/auth/logout` |
| Users | `/users`, `/users/:id/profile`, `/users/profile_batch`, `/users/:id`, `/users/:id/banner`, `/users/:id/ban`, `/users/settings` |
| Servers | `/server` |
| Channels | `/channels`, `/channels/:id`, `/channels/:id/change_position`, `/channels/:id/permissions` |
| Messages | `/channels/:id/messages`, `/messages`, `/messages/:id` |
| Roles | `/roles` |
| Emojis | `/emojis`, `/emojis/:id` |
| Link Preview (Embedding) | `/link-previews/{preview_id}`|
| Attachments | `/attachments/:id` `/attachments/:id/thumbnail` |
| Media | `/media/:sha_hash` |
| Search | `/search` |
| Logs | `/admin/audit-logs` |

Erros seguem RFC 7807 (`application/problem+json`), com header `X-Request-ID` em toda resposta.

## WebSocket

Autenticação via o mesmo cookie `Auth` da API REST, validado no handshake.

| Evento | Direção |
|---|---|
| `message`, `message_edit`, `message_delete`, `message_pin` | outbound |
| `new_preview`, `remove_preview` | outbound |
| `channel_create`, `channel_update`, `channel_delete` | outbound |
| `typing` | inbound / outbound |
| `presence_sync` | unicast (snapshot no connect) |
| `presence_update` | outbound (delta) |
| `user_join` | outbound |
| `avatar_update` | outbound |
| `reaction_update` | outbound |
| `heartbeat` / `heartbeat_ack` | inbound / outbound |
| `error` | outbound |

Presença e digitação são estado efêmero, mantido só em memória — nunca persistido no Postgres.

## Segurança

- Validação de arquivo por magic number, nunca por extensão ou `Content-Type` declarado
- Rate limiting por IP e usuário
- Permission checks por role em cada endpoint sensível
- Proteção contra SSRF em qualquer request de saída (link preview)
- Hook plugável para verificação de CSAM (recomendação padrão: Cloudflare CSAM Scanning Tool)

## Testes

```bash
go test ./...
```

## Licença

AGPLv3

## AI Disclaimer

Esse projeto foi desenvolvido com a assistência do Qwen3.8-27B, uma IA Local com um custo total de $0 em tokens, usando ferramentas OpenSource sempre que possível.

Foi usada a técnica human-in-the-loop através de um estrito ROADMAP, AGENTS.md e política de segurança, corrigindo e opinando no trabalho da IA sempre que necessário.

[Qwen Team. *Qwen3.8-27B: A New Bar for Coding and Cowork*. August 2026.](https://qwen.ai/home)
