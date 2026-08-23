[![License: AGPL v3](https://img.shields.io/badge/License-AGPLv3-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)
[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go)](https://go.dev)
[![Release](https://img.shields.io/github/v/release/jonasslv/papo-backend)](https://github.com/jonasslv/papo-backend/releases)
# Papo — Backend

Backend de um chat self-hosted, inspirado no Discord dos primeiros anos: simples, leve e sem microserviços. Um binário Go, um Postgres, um servidor por instância.

## Stack

- **Go** + **Echo** — HTTP
- **PGX** — PostgreSQL
- **Gorilla WebSocket** — realtime
- **JWT** + **Bcrypt** — autenticação
- **Goose** — migrations
- **Logrus** — logging

## Roadmap

- [x] Backend MVP completo (endpoints, websocket, com segurança, testes e performance)
- [ ] Processamento Thumbnail no backend (com oEmbed, Opengraph, segurança reforçada)
- [ ] Processamento Icons no Backend 
- [ ] User Profile (Descrição, banner)
- [ ] Cleanup (excluir código não usado)
- [ ] Crons GC Attachments orfãos/tabela quebrada
- [ ] Status Ausente/Ocupado (novo field)
- [ ] Suporte WebRTC (Audio, Video, Transmissão)
- [ ] React Mensagens
- [ ] Direct Messages
- [ ] Moderação Automatica (Nudez/Gore)
- [ ] Bot API
- [ ] Suporte NoSQL (intercambeavel nas settings) ou particionamento postgresql
- [ ] Hashed Resync para conexão instável

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

## Rodando localmente

Pré-requisitos: Go 1.21+, Docker (para o Postgres), [Goose](https://github.com/pressly/goose).

```bash
# banco
cd infra && docker-compose up -d

# migrations
cd ../migrations && goose up

# backend
cd ../backend
go mod tidy
go run cmd/main.go
```

Backend sobe em `http://localhost:8080`, WebSocket em `ws://localhost:8080/ws`.

## API

Esquema completo em [`openapi.yml`](./openapi.yml).

| Recurso | Endpoints principais |
|---|---|
| Auth | `/auth/register`, `/auth/login`, `/auth/loginServer`, `/auth/whoami`, `/auth/logout` |
| Users | `/users`, `/users/profile`, `/users/:id`, `/users/:id/ban`, `/users/settings` |
| Servers | `/servers`, `/servers/:id`, `/servers/:id/roles` |
| Channels | `/channels`, `/channels/:id`, `/channels/:id/change_position`, `/channels/:id/permissions` |
| Messages | `/channels/:id/messages`, `/messages`, `/messages/:id` |
| Emojis | `/emojis`, `/emojis/:id` |
| Attachments | `/attachments/:id` |
| Search | `/search` |

Erros seguem RFC 7807 (`application/problem+json`), com header `X-Request-ID` em toda resposta.

## WebSocket

Autenticação via o mesmo cookie `Auth` da API REST, validado no handshake.

| Evento | Direção |
|---|---|
| `message`, `message_edit`, `message_delete` | outbound |
| `channel_create`, `channel_update`, `channel_delete` | outbound |
| `typing` | inbound / outbound |
| `presence_sync` | unicast (snapshot no connect) |
| `presence_update` | outbound (delta) |
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
