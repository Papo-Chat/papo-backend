package handlers

import (
    "encoding/json"
 "context"
 "papo/internal/websocket"
    "errors"
    "net/http"
    "strings"

    "papo/internal/middleware"
    "papo/internal/models"
    "papo/internal/services"
    "papo/internal/utils"

    "github.com/labstack/echo/v4"
)

type createWebhookRequest struct {
    ChannelID string `json:"channel_id"`
    Name string `json:"name"`
}
type createWebhookResponse struct {
    models.Webhook
    Token string `json:"token"`
}

func CreateWebhookHandler(baseURL string,c echo.Context) error {
    actor,_:=c.Get(middleware.UserIDContextKey).(string)
    var req createWebhookRequest
    if err:=c.Bind(&req);err!=nil {return utils.SendProblem(c,baseURL,http.StatusBadRequest,"invalid-param","Parâmetro inválido","JSON inválido")}
    webhook,token,err:=services.CreateWebhook(c.Request().Context(),actor,req.ChannelID,req.Name)
    if err!=nil{return webhookProblem(c,baseURL,err)}
    return c.JSON(http.StatusCreated,createWebhookResponse{Webhook:webhook,Token:token})
}
func ListWebhooksHandler(baseURL string,c echo.Context) error {
    items,err:=services.ListWebhooks(c.Request().Context())
    if err!=nil {return webhookProblem(c,baseURL,err)}
    return c.JSON(http.StatusOK,map[string]any{"webhooks":items})
}
func DeleteWebhookHandler(baseURL string,c echo.Context) error {
    if err:=services.RevokeWebhook(c.Request().Context(),c.Param("webhook_id"));err!=nil{return webhookProblem(c,baseURL,err)}
    return c.NoContent(http.StatusNoContent)
}
func webhookProblem(c echo.Context,baseURL string,err error) error {
    switch {
    case errors.Is(err,services.ErrWebhookUnauthorized):
        return utils.SendProblem(c,baseURL,http.StatusUnauthorized,"unauthorized","Não autorizado","webhook inválido")
    case errors.Is(err,services.ErrWebhookNotFound),errors.Is(err,services.ErrChannelNotFound),errors.Is(err,services.ErrMessageNotFound):
        return utils.SendProblem(c,baseURL,http.StatusNotFound,"not-found","Não encontrado","recurso inexistente")
    case errors.Is(err,services.ErrPermissionDenied),errors.Is(err,services.ErrDirectMessageBlocked):
        return utils.SendProblem(c,baseURL,http.StatusForbidden,"forbidden","Acesso negado","operação não permitida")
    case errors.Is(err,services.ErrInvalidInput),errors.Is(err,services.ErrInvalidEmbed),errors.Is(err,services.ErrInvalidExternalAuthor):
        return utils.SendProblem(c,baseURL,http.StatusBadRequest,"invalid-param","Parâmetro inválido",err.Error())
    default:
        return utils.SendProblem(c,baseURL,http.StatusInternalServerError,"internal","Erro interno","falha no webhook")
    }
}

// ExecuteWebhookHandler is an incoming message. It uses a scoped webhook token,
// never the JWT of a bot account. The handler shares post-creation side effects
// with user messages and does not replay them for idempotent duplicate requests.
func ExecuteWebhookHandler(baseURL string,c echo.Context) error {
    token:=strings.TrimPrefix(c.Request().Header.Get("Authorization"),"Bearer ")
    if token=="" || token==c.Request().Header.Get("Authorization") {
        return utils.SendProblem(c,baseURL,http.StatusUnauthorized,"unauthorized","Não autorizado","token de webhook ausente")
    }
    hook,err:=services.AuthenticateWebhook(c.Request().Context(),c.Param("webhook_id"),token)
    if err!=nil{return webhookProblem(c,baseURL,err)}
    if err:=c.Request().ParseMultipartForm(110 << 20);err!=nil {
        return utils.SendProblem(c,baseURL,http.StatusBadRequest,"invalid-param","Parâmetro inválido","multipart inválido")
    }
    content:=c.FormValue("content")
    sourceID:=c.FormValue("external_message_id")
    replyTo:=c.FormValue("reply_to")
    var external *models.ExternalAuthor
    if raw:=c.FormValue("external_author");raw!="" {
        external=new(models.ExternalAuthor)
        if err:=json.Unmarshal([]byte(raw),external);err!=nil {
            return utils.SendProblem(c,baseURL,http.StatusBadRequest,"invalid-param","Parâmetro inválido","external_author inválido")
        }
    }
    embeds,err:=parseEmbedsField(c.FormValue("embeds"))
    if err!=nil{return utils.SendProblem(c,baseURL,http.StatusBadRequest,"invalid-param","Parâmetro inválido","embeds inválidos")}
    var attachments []services.AttachmentInput
    if c.Request().MultipartForm!=nil {
        for _,h:=range c.Request().MultipartForm.File["attachments"] {
            f,err:=h.Open()
            if err!=nil {return utils.SendProblem(c,baseURL,http.StatusBadRequest,"invalid-param","Parâmetro inválido","anexo inválido")}
            defer f.Close()
            attachments=append(attachments,services.AttachmentInput{OriginalFileName:h.Filename,Content:f})
        }
    }
    message,created,err:=services.CreateWebhookMessage(c.Request().Context(),hook,external,sourceID,content,replyTo,attachments,embeds)
    if err!=nil{return webhookProblem(c,baseURL,err)}
    if created {publishCreatedMessage(c,message,hook.CreatedBy)}
    status:=http.StatusCreated
    if !created {status=http.StatusOK}
    return c.JSON(status,message)
}



type webhookEditRequest struct {
    Content string `json:"content"`
    Embeds []services.EmbedInput `json:"embeds"`
}
func webhookFromRequest(baseURL string,c echo.Context)(models.Webhook,error){
    raw:=c.Request().Header.Get("Authorization")
    if !strings.HasPrefix(raw,"Bearer ") {return models.Webhook{},services.ErrWebhookUnauthorized}
    return services.AuthenticateWebhook(c.Request().Context(),c.Param("webhook_id"),strings.TrimPrefix(raw,"Bearer "))
}
func EditWebhookMessageHandler(baseURL string,c echo.Context)error {
    w,err:=webhookFromRequest(baseURL,c)
    if err!=nil{return webhookProblem(c,baseURL,err)}
    var req webhookEditRequest
    if err:=c.Bind(&req);err!=nil{return utils.SendProblem(c,baseURL,http.StatusBadRequest,"invalid-param","Parâmetro inválido","JSON inválido")}
    message,err:=services.EditWebhookMessage(c.Request().Context(),w,c.Param("message_id"),req.Content,req.Embeds)
    if err!=nil{return webhookProblem(c,baseURL,err)}
    broadcastChannelEvent(c,message.ChannelID,websocket.MessageEditOutbound{
        Type:websocket.EventTypeMessageEdit,ID:message.ID,ChannelID:message.ChannelID,
        Content:derefString(message.Content),EditedAt:derefTime(message.EditedAt),
    })
    requestID:=c.Request().Header.Get(echo.HeaderXRequestID)
    broadcastMessageEmbedsUpdate(c.Request().Context(),requestID,message.ChannelID,message.ID)
    go processEditedMessageEmbeds(context.Background(),requestID,message.ChannelID,message.ID,w.CreatedBy,derefString(message.Content),message.Embeds)
    return c.JSON(http.StatusOK,message)
}
func DeleteWebhookMessageHandler(baseURL string,c echo.Context)error {
    w,err:=webhookFromRequest(baseURL,c)
    if err!=nil{return webhookProblem(c,baseURL,err)}
    channelID,err:=services.DeleteWebhookMessage(c.Request().Context(),w,c.Param("message_id"))
    if err!=nil{return webhookProblem(c,baseURL,err)}
    broadcastChannelEvent(c,channelID,websocket.MessageDeleteOutbound{
        Type:websocket.EventTypeMessageDelete,ID:c.Param("message_id"),ChannelID:channelID,
    })
    return c.NoContent(http.StatusNoContent)
}
