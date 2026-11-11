package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/mercadopago"
)

var segredo = []byte("segredo-de-teste-com-mais-de-32-caracteres")

const segredoWebhook = "segredo-webhook-de-teste"

func token(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	claims["aud"], claims["exp"] = audiencia, time.Now().Add(time.Hour).Unix()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(segredo)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type ucFake struct {
	o          orcamento.Orcamento
	p          *orcamento.Pagamento
	err        error
	ator       cobranca.Ator
	motivo     string
	confirmada string
}

func (u *ucFake) Consultar(_ context.Context, _ uuid.UUID, a cobranca.Ator) (orcamento.Orcamento, *orcamento.Pagamento, error) {
	u.ator = a
	return u.o, u.p, u.err
}
func (u *ucFake) ConsultarPorOS(_ context.Context, _ uuid.UUID, a cobranca.Ator) (orcamento.Orcamento, *orcamento.Pagamento, error) {
	u.ator = a
	return u.o, u.p, u.err
}
func (u *ucFake) Aprovar(_ context.Context, _ uuid.UUID, a cobranca.Ator) (orcamento.Orcamento, orcamento.Pagamento, error) {
	u.ator = a
	if u.p == nil {
		return u.o, orcamento.Pagamento{}, u.err
	}
	return u.o, *u.p, u.err
}
func (u *ucFake) Recusar(_ context.Context, _ uuid.UUID, a cobranca.Ator, motivo string) (orcamento.Orcamento, error) {
	u.ator, u.motivo = a, motivo
	return u.o, u.err
}
func (u *ucFake) ConfirmarCobranca(_ context.Context, order string) error {
	u.confirmada = order
	return u.err
}

func fixture() (orcamento.Orcamento, *orcamento.Pagamento) {
	o, _ := orcamento.Novo(uuid.New(), uuid.New(), "OS-1", orcamento.Cliente{}, "diag",
		[]orcamento.Item{{Tipo: "peca", Nome: "Filtro", Quantidade: 2, PrecoUnitario: 2890}}, time.Now())
	p := &orcamento.Pagamento{ID: uuid.New(), Status: orcamento.PagamentoPendente, CheckoutURL: "http://mp/x", Valor: o.Total, MPOrderID: "ORD1"}
	return o, p
}

func rotas(uc *ucFake) http.Handler {
	return Rotas(uc, Config{SegredoJWT: segredo, SegredoWebhook: segredoWebhook,
		Pronto: func(context.Context) error { return nil }})
}

func chamar(t *testing.T, h http.Handler, metodo, caminho, tok, corpo string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(metodo, caminho, strings.NewReader(corpo))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealth(t *testing.T) {
	h := rotas(&ucFake{})
	if r := chamar(t, h, "GET", "/health/live", "", ""); r.Code != 200 {
		t.Errorf("live = %d", r.Code)
	}
	if r := chamar(t, h, "GET", "/health/ready", "", ""); r.Code != 200 {
		t.Errorf("ready = %d", r.Code)
	}
	fora := Rotas(&ucFake{}, Config{SegredoJWT: segredo, Pronto: func(context.Context) error { return errors.New("db fora") }})
	if r := chamar(t, fora, "GET", "/health/ready", "", ""); r.Code != 503 {
		t.Errorf("ready com banco fora = %d", r.Code)
	}
}

func TestConsultarEPorOS(t *testing.T) {
	o, p := fixture()
	uc := &ucFake{o: o, p: p}
	h := rotas(uc)
	cli := token(t, jwt.MapClaims{"sub": "c1", "tipo": "cliente"})

	r := chamar(t, h, "GET", "/billing/v1/orcamentos/"+o.ID.String(), cli, "")
	var corpo orcamentoJSON
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &corpo) != nil {
		t.Fatalf("consultar = %d %s", r.Code, r.Body)
	}
	if corpo.ValorTotal != "57.80" || corpo.Itens[0].Subtotal != "57.80" || corpo.Pagamento.LinkPagamento != "http://mp/x" {
		t.Errorf("corpo = %+v", corpo)
	}
	if uc.ator.Tipo != "cliente" || uc.ator.Sub != "c1" {
		t.Errorf("ator repassado = %+v", uc.ator)
	}
	if r := chamar(t, h, "GET", "/billing/v1/orcamentos?os_id="+o.OsID.String(), cli, ""); r.Code != 200 {
		t.Errorf("por OS = %d", r.Code)
	}
	if r := chamar(t, h, "GET", "/billing/v1/orcamentos?os_id=x", cli, ""); r.Code != 400 {
		t.Errorf("os_id inválido = %d", r.Code)
	}
	if r := chamar(t, h, "GET", "/billing/v1/orcamentos/nao-uuid", cli, ""); r.Code != 400 {
		t.Errorf("id inválido = %d", r.Code)
	}
	if r := chamar(t, h, "GET", "/billing/v1/orcamentos/"+o.ID.String(), "", ""); r.Code != 401 {
		t.Errorf("sem token = %d", r.Code)
	}
	if r := chamar(t, h, "GET", "/billing/v1/orcamentos/"+o.ID.String(), "a.b.c", ""); r.Code != 401 {
		t.Errorf("token inválido = %d", r.Code)
	}
}

func TestAprovarERecusar(t *testing.T) {
	o, p := fixture()
	uc := &ucFake{o: o, p: p}
	h := rotas(uc)
	fun := token(t, jwt.MapClaims{"sub": "u1", "tipo": "usuario"})

	r := chamar(t, h, "POST", "/billing/v1/orcamentos/"+o.ID.String()+"/aprovar", fun, "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"link_pagamento":"http://mp/x"`) {
		t.Fatalf("aprovar = %d %s", r.Code, r.Body)
	}
	if r := chamar(t, h, "POST", "/billing/v1/orcamentos/"+o.ID.String()+"/recusar", fun, `{"motivo":"caro"}`); r.Code != 200 || uc.motivo != "caro" {
		t.Errorf("recusar = %d (motivo %q)", r.Code, uc.motivo)
	}
	if r := chamar(t, h, "POST", "/billing/v1/orcamentos/"+o.ID.String()+"/recusar", fun, ``); r.Code != 200 {
		t.Errorf("recusar sem corpo (motivo opcional) = %d", r.Code)
	}
	for _, rota := range []string{"/aprovar", "/recusar"} {
		if r := chamar(t, h, "POST", "/billing/v1/orcamentos/nao-uuid"+rota, fun, ""); r.Code != 400 {
			t.Errorf("%s com id inválido = %d", rota, r.Code)
		}
	}
}

func TestErrosTraduzidos(t *testing.T) {
	o, _ := fixture()
	fun := token(t, jwt.MapClaims{"sub": "u1"})
	for _, c := range []struct {
		err    error
		status int
	}{
		{cobranca.ErrNaoEncontrado, 404}, {cobranca.ErrProibido, 403},
		{orcamento.ErrEstadoInvalido, 409}, {cobranca.ErrConflito, 409}, {errors.New("mp fora"), 502},
	} {
		h := rotas(&ucFake{o: o, err: c.err})
		for _, rota := range []struct{ metodo, caminho string }{
			{"GET", "/billing/v1/orcamentos/" + o.ID.String()},
			{"GET", "/billing/v1/orcamentos?os_id=" + o.OsID.String()},
			{"POST", "/billing/v1/orcamentos/" + o.ID.String() + "/aprovar"},
			{"POST", "/billing/v1/orcamentos/" + o.ID.String() + "/recusar"},
		} {
			if r := chamar(t, h, rota.metodo, rota.caminho, fun, "{}"); r.Code != c.status {
				t.Errorf("%v em %s %s: %d, esperava %d", c.err, rota.metodo, rota.caminho, r.Code, c.status)
			}
		}
	}
}

func notificacao(t *testing.T, h http.Handler, tipo, dataID, segredoUsado string, ts time.Time) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/billing/webhooks/mercadopago?data.id="+dataID+"&type="+tipo, strings.NewReader(`{}`))
	tss := strconv.FormatInt(ts.Unix(), 10)
	req.Header.Set("x-request-id", "req-1")
	req.Header.Set("x-signature", "ts="+tss+",v1="+mercadopago.Assinar(segredoUsado, "req-1", dataID, tss))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhook(t *testing.T) {
	uc := &ucFake{}
	h := rotas(uc)
	if r := notificacao(t, h, "order", "ORDTST1", segredoWebhook, time.Now()); r.Code != 200 || uc.confirmada != "ORDTST1" {
		t.Fatalf("webhook válido = %d (confirmada %q)", r.Code, uc.confirmada)
	}
	if r := notificacao(t, h, "order", "ORDTST1", "segredo-errado", time.Now()); r.Code != 401 {
		t.Errorf("assinatura inválida = %d", r.Code)
	}
	uc.confirmada = ""
	if r := notificacao(t, h, "payment", "123", segredoWebhook, time.Now()); r.Code != 200 || uc.confirmada != "" {
		t.Errorf("tipo não tratado: %d (não deve confirmar)", r.Code)
	}
	desconhecida := rotas(&ucFake{err: cobranca.ErrNaoEncontrado})
	if r := notificacao(t, desconhecida, "order", "ORDX", segredoWebhook, time.Now()); r.Code != 200 {
		t.Errorf("order desconhecida (simulação do painel) = %d", r.Code)
	}
	fora := rotas(&ucFake{err: errors.New("mp fora")})
	if r := notificacao(t, fora, "order", "ORDX", segredoWebhook, time.Now()); r.Code != 500 {
		t.Errorf("falha na confirmação deve devolver 500 para o MP reenviar: %d", r.Code)
	}
}

func TestSegredoJWT(t *testing.T) {
	t.Setenv("JWT_SECRET", "curto")
	if _, err := SegredoJWT(); err == nil {
		t.Error("segredo curto")
	}
	t.Setenv("JWT_SECRET", string(segredo))
	if s, err := SegredoJWT(); err != nil || string(s) != string(segredo) {
		t.Errorf("SegredoJWT = %v", err)
	}
}
