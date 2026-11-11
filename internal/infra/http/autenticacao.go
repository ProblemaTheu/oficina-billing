// Package http expõe a API do Billing (/billing/v1), o webhook do Mercado Pago
// e os probes de saúde.
package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/application/logging"
)

// O token é o mesmo do OS Service (contrato F3-0.2): HS256 com o segredo
// compartilhado, aud=oficina-api, tipo=usuario|cliente.
const (
	audiencia    = "oficina-api"
	tamanhoChave = 32
)

type ctxClaims struct{}

// SegredoJWT lê JWT_SECRET e falha alto se ausente ou curto.
func SegredoJWT() ([]byte, error) {
	v := os.Getenv("JWT_SECRET")
	if len(v) < tamanhoChave {
		return nil, fmt.Errorf("JWT_SECRET ausente ou com menos de %d caracteres", tamanhoChave)
	}
	return []byte(v), nil
}

func autenticar(segredo []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cabecalho := r.Header.Get("Authorization")
			if !strings.HasPrefix(cabecalho, "Bearer ") {
				erroJSON(w, http.StatusUnauthorized, "nao_autorizado", "token ausente ou mal formatado")
				return
			}
			token, err := jwt.Parse(strings.TrimPrefix(cabecalho, "Bearer "), func(*jwt.Token) (any, error) {
				return segredo, nil
			}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(audiencia))
			if err != nil || !token.Valid {
				erroJSON(w, http.StatusUnauthorized, "nao_autorizado", "token inválido ou expirado")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxClaims{}, token.Claims.(jwt.MapClaims))))
		})
	}
}

// ator transforma os claims no Ator do caso de uso.
func ator(ctx context.Context) cobranca.Ator {
	c, _ := ctx.Value(ctxClaims{}).(jwt.MapClaims)
	tipo, _ := c["tipo"].(string)
	sub, _ := c["sub"].(string)
	return cobranca.Ator{Tipo: tipo, Sub: sub}
}

// cabecalhoCorrelacao é o mesmo header do OS Service.
const cabecalhoCorrelacao = "X-Correlation-Id"

func correlacao(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(cabecalhoCorrelacao)
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set(cabecalhoCorrelacao, id)
		log := logging.Log(r.Context()).With("correlation_id", id, "http.method", r.Method, "http.route", r.URL.Path)
		next.ServeHTTP(w, r.WithContext(logging.ComCorrelacao(logging.ComLogger(r.Context(), log), id)))
	})
}

func erroJSON(w http.ResponseWriter, status int, codigo, mensagem string) {
	escreverJSON(w, status, map[string]string{"code": codigo, "message": mensagem})
}

func escreverJSON(w http.ResponseWriter, status int, corpo any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(corpo)
}
