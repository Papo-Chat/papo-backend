-- +goose Up
-- Auth híbrida (JWT stateless + revogação no banco):
-- user_connections guarda uma row por conexão de autenticação ATIVA do
-- usuário. O banco guarda SOMENTE o hash SHA-256 do token (token_hash),
-- nunca o token. token_issued_at é o iat do JWT: como o token é uma função
-- pura de (user_id, iat, exp, segredo), o servidor re-deriva o token na
-- janela de graça da rotação sem precisar guardá-lo em claro.
--
-- Rotação: o token antigo recebe replaced_at/replaced_by e o token novo
-- entra como nova row. A history guarda as rows substituídas (arquivadas
-- pela rotina de manutenção) para que o reuso de um token antigo continue
-- detectável mesmo depois de sair da tabela ativa.
--
-- connection_violation: marcado quando o reuso de token é detectado (o
-- cliente usa a flag para avisar o usuário); limpo na troca de senha.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS connection_violation BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS user_connections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL,
    token_issued_at TIMESTAMPTZ NOT NULL,
    replaced_at TIMESTAMPTZ,
    replaced_by UUID REFERENCES user_connections(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, token_hash)
);

CREATE TABLE IF NOT EXISTS user_connections_history (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    token_hash TEXT NOT NULL,
    token_issued_at TIMESTAMPTZ NOT NULL,
    replaced_at TIMESTAMPTZ NOT NULL,
    replaced_by UUID,
    created_at TIMESTAMPTZ NOT NULL
);

-- A UNIQUE (user_id, token_hash) já indexa a consulta por token do
-- middleware (WHERE user_id = ? AND token_hash = ?) e a listagem por
-- usuário (prefixo user_id).
-- Arquivamento pela manutenção (rows substituídas antigas).
CREATE INDEX IF NOT EXISTS idx_user_connections_replaced_at ON user_connections (replaced_at) WHERE replaced_at IS NOT NULL;
-- Detecção de reuso na history (WHERE user_id = ? AND token_hash = ?).
CREATE INDEX IF NOT EXISTS idx_user_connections_history_user_hash ON user_connections_history (user_id, token_hash);

-- +goose Down
DROP INDEX IF EXISTS idx_user_connections_history_user_hash;
DROP INDEX IF EXISTS idx_user_connections_replaced_at;
DROP TABLE IF EXISTS user_connections_history;
DROP TABLE IF EXISTS user_connections;
ALTER TABLE users DROP COLUMN IF EXISTS connection_violation;
