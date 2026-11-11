package mercadopago

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// JanelaAntiReplay é a idade máxima aceita para o ts do x-signature.
const JanelaAntiReplay = 5 * time.Minute

// AssinaturaValida confere o x-signature de um webhook do Mercado Pago.
//
// Formato validado em 10/11 contra notificações do painel: header
// `ts=<segundos>,v1=<hmac-sha256 hex>`; manifesto
// `id:<data.id>;request-id:<x-request-id>;ts:<ts>;`, com data.id vindo da query
// string e em minúsculas (ids de order são alfanuméricos maiúsculos, ORDTST...).
func AssinaturaValida(segredo, xSignature, xRequestID, dataID string, agora time.Time) bool {
	var ts, v1 string
	for _, parte := range strings.Split(xSignature, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(parte), "=")
		switch k {
		case "ts":
			ts = v
		case "v1":
			v1 = v
		}
	}
	if segredo == "" || ts == "" || v1 == "" || dataID == "" {
		return false
	}
	seg, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || agora.Sub(time.Unix(seg, 0)).Abs() > JanelaAntiReplay {
		return false
	}
	return hmac.Equal([]byte(Assinar(segredo, xRequestID, dataID, ts)), []byte(v1))
}

// Assinar calcula o v1 — usado pelo stub para assinar as notificações que envia.
func Assinar(segredo, xRequestID, dataID, ts string) string {
	mac := hmac.New(sha256.New, []byte(segredo))
	_, _ = fmt.Fprintf(mac, "id:%s;request-id:%s;ts:%s;", strings.ToLower(dataID), xRequestID, ts)
	return hex.EncodeToString(mac.Sum(nil))
}
