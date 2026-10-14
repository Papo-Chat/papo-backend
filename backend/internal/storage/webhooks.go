package storage

import (
    "context"
    "fmt"
    "papo/internal/models"
)

const webhookColumns = "id, channel_id, name, created_by, created_at, revoked_at"

func scanWebhook(row rowScanner) (models.Webhook, error) {
    var w models.Webhook
    err := row.Scan(&w.ID, &w.ChannelID, &w.Name, &w.CreatedBy, &w.CreatedAt, &w.RevokedAt)
    if err != nil { return models.Webhook{}, mapStorageError(err) }
    return w, nil
}

func CreateWebhook(ctx context.Context, channelID, name, creator, secretHash string) (models.Webhook, error) {
    return scanWebhook(GetDB().QueryRowContext(ctx,
        "INSERT INTO webhooks (channel_id, name, created_by, secret_hash) VALUES ($1,$2,$3,$4) RETURNING "+webhookColumns,
        channelID,name,creator,secretHash))
}

func ListWebhooks(ctx context.Context) ([]models.Webhook, error) {
    rows, err := GetDB().QueryContext(ctx,"SELECT "+webhookColumns+" FROM webhooks WHERE revoked_at IS NULL ORDER BY created_at DESC")
    if err != nil { return nil, err }
    defer rows.Close()
    out := make([]models.Webhook,0)
    for rows.Next() { w, err := scanWebhook(rows); if err != nil { return nil,err }; out=append(out,w) }
    return out,rows.Err()
}

func GetActiveWebhookWithHash(ctx context.Context, id string) (models.Webhook, string, error) {
    var hash string
    var w models.Webhook
    err := GetDB().QueryRowContext(ctx,
        "SELECT "+webhookColumns+", secret_hash FROM webhooks WHERE id=$1 AND revoked_at IS NULL",id).
        Scan(&w.ID,&w.ChannelID,&w.Name,&w.CreatedBy,&w.CreatedAt,&w.RevokedAt,&hash)
    if err != nil { return models.Webhook{},"",mapStorageError(err) }
    return w,hash,nil
}

func RevokeWebhook(ctx context.Context,id string) error {
    result,err := GetDB().ExecContext(ctx,"UPDATE webhooks SET revoked_at=NOW() WHERE id=$1 AND revoked_at IS NULL",id)
    if err != nil { return fmt.Errorf("revoke webhook: %w",err) }
    if n,_ := result.RowsAffected(); n==0 { return ErrNotFound }
    return nil
}

func GetWebhookMessage(ctx context.Context,webhookID,sourceID string) (models.Message,error) {
    message,err:=scanMessage(GetDB().QueryRowContext(ctx,
        "SELECT "+messageColumns+" FROM messages WHERE webhook_id=$1 AND external_message_id=$2",webhookID,sourceID))
    if err!=nil {return models.Message{},mapStorageError(err)}
    return message,nil
}


