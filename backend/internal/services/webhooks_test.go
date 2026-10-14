package services

import (
    "testing"
    "papo/internal/models"
)

func TestValidateExternalAuthor(t *testing.T) {
    cases:=[]struct{name string;author *models.ExternalAuthor;wantError bool}{
        {"no attribution",nil,false},
        {"discord",&models.ExternalAuthor{Source:"discord",ID:"123",Name:"Original author",AvatarURL:"https://cdn.discordapp.com/avatar.png"},false},
        {"irc",&models.ExternalAuthor{Source:"irc",ID:"nickname!host",Name:"Nickname"},false},
        {"missing id",&models.ExternalAuthor{Source:"discord",Name:"Me"},true},
        {"missing source",&models.ExternalAuthor{ID:"123",Name:"Me"},true},
        {"bad source",&models.ExternalAuthor{Source:"DISCORD",ID:"123",Name:"Me"},true},
        {"control character",&models.ExternalAuthor{Source:"discord",ID:"123",Name:"Me\nInjected"},true},
        {"local avatar",&models.ExternalAuthor{Source:"discord",ID:"123",Name:"Me",AvatarURL:"http://localhost:3000/private"},true},
    }
    for _,tc:=range cases {
        t.Run(tc.name,func(t *testing.T){
            got:=validateExternalAuthor(tc.author)
            if (got!=nil)!=tc.wantError {t.Fatalf("got error %v; want error=%v",got,tc.wantError)}
        })
    }
}
