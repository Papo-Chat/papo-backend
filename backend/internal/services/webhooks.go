package services

import (
    "context"
    "crypto/rand"
    "crypto/sha256"
    "crypto/subtle"
    "encoding/hex"
    "errors"
    "strings"
    "unicode/utf8"

    "papo/internal/models"
    "papo/internal/storage"
)

var ErrWebhookNotFound = errors.New("webhook não encontrado")
var ErrWebhookUnauthorized = errors.New("webhook não autorizado")
var ErrInvalidExternalAuthor = errors.New("autoria externa inválida")

func CreateWebhook(ctx context.Context,actorID,channelID,name string) (models.Webhook,string,error) {
    if strings.TrimSpace(name)=="" || utf8.RuneCountInString(name)>80 { return models.Webhook{},"",ErrInvalidInput }
    channel,err:=storage.GetChannelByID(ctx,channelID)
    if errors.Is(err,storage.ErrNotFound) { return models.Webhook{},"",ErrChannelNotFound }
    if err!=nil { return models.Webhook{},"",err }
    if channel.Type!="text" && channel.Type!="voice" { return models.Webhook{},"",ErrPermissionDenied }
    tokenBytes:=make([]byte,32)
    if _,err:=rand.Read(tokenBytes);err!=nil { return models.Webhook{},"",err }
    secret:=hex.EncodeToString(tokenBytes)
    hash:=sha256.Sum256([]byte(secret))
    webhook,err:=storage.CreateWebhook(ctx,channelID,name,actorID,hex.EncodeToString(hash[:]))
    if err==nil {
        RecordAudit(ctx,AuditEntry{ActorID:actorID,Action:"webhook.create",EntityType:"webhook",EntityID:&webhook.ID,
            Metadata:map[string]any{"channel_id":channelID,"name":name}})
    }
    return webhook,secret,err
}

func ListWebhooks(ctx context.Context) ([]models.Webhook,error) {return storage.ListWebhooks(ctx)}

func RevokeWebhook(ctx context.Context,id string) error {
    err:=storage.RevokeWebhook(ctx,id)
    if errors.Is(err,storage.ErrNotFound) { return ErrWebhookNotFound }
    return err
}

func AuthenticateWebhook(ctx context.Context,id,token string) (models.Webhook,error) {
    w,hash,err:=storage.GetActiveWebhookWithHash(ctx,id)
    if err!=nil { return models.Webhook{},ErrWebhookUnauthorized }
    sum:=sha256.Sum256([]byte(token))
    actual:=hex.EncodeToString(sum[:])
    if subtle.ConstantTimeCompare([]byte(actual),[]byte(hash))!=1 { return models.Webhook{},ErrWebhookUnauthorized }
    return w,nil
}

func validateExternalAuthor(a *models.ExternalAuthor) error {
    if a==nil { return nil }
    if strings.TrimSpace(a.Source)=="" || len(a.Source)>40 || strings.TrimSpace(a.ID)=="" ||
       len(a.ID)>200 || strings.TrimSpace(a.Name)=="" || utf8.RuneCountInString(a.Name)>100 {return ErrInvalidExternalAuthor}
    for _,ch:=range a.Source { if !(ch>='a'&&ch<='z'||ch>='0'&&ch<='9'||ch=='_'||ch=='-') {return ErrInvalidExternalAuthor} }
    for _,ch:=range a.Name {if ch<0x20 || ch==0x7f {return ErrInvalidExternalAuthor}}
    if a.AvatarURL!="" {if _,err:=validateEmbedURL(a.AvatarURL);err!=nil {return ErrInvalidExternalAuthor}}
    if a.ProfileURL!="" {if _,err:=validateEmbedURL(a.ProfileURL);err!=nil {return ErrInvalidExternalAuthor}}
    return nil
}
