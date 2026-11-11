package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/application/logging"
	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/mercadopago"
)

// casosDeUso é o que a API usa do cobranca.Servico.
type casosDeUso interface {
	Consultar(ctx context.Context, id uuid.UUID, a cobranca.Ator) (orcamento.Orcamento, *orcamento.Pagamento, error)
	ConsultarPorOS(ctx context.Context, osID uuid.UUID, a cobranca.Ator) (orcamento.Orcamento, *orcamento.Pagamento, error)
	Aprovar(ctx context.Context, id uuid.UUID, a cobranca.Ator) (orcamento.Orcamento, orcamento.Pagamento, error)
	Recusar(ctx context.Context, id uuid.UUID, a cobranca.Ator, motivo string) (orcamento.Orcamento, error)
	ConfirmarCobranca(ctx context.Context, orderID string) error
}

// Config da API.
type Config struct {
	SegredoJWT     []byte
	SegredoWebhook string
	Pronto         func(ctx context.Context) error
	Agora          func() time.Time // nil = time.Now
}

// Rotas monta o roteador.
func Rotas(uc casosDeUso, cfg Config) http.Handler {
	if cfg.Agora == nil {
		cfg.Agora = time.Now
	}
	r := chi.NewRouter()
	r.Use(correlacao)
	r.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		escreverJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	})
	r.Get("/health/ready", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancelar := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancelar()
		if err := cfg.Pronto(ctx); err != nil {
			logging.Log(ctx).Warn("health: dependência indisponível", "error", err)
			escreverJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "DOWN", "components": map[string]string{"db": "DOWN"}})
			return
		}
		escreverJSON(w, http.StatusOK, map[string]any{"status": "UP", "components": map[string]string{"db": "UP"}})
	})

	h := handlers{uc: uc}
	// Webhook público (sem JWT no Gateway): a autenticidade vem do x-signature.
	r.Post("/billing/webhooks/mercadopago", webhook(uc, cfg.SegredoWebhook, cfg.Agora))
	r.Route("/billing/v1", func(r chi.Router) {
		r.Use(autenticar(cfg.SegredoJWT))
		r.Get("/orcamentos", h.porOS)
		r.Get("/orcamentos/{id}", h.consultar)
		r.Post("/orcamentos/{id}/aprovar", h.aprovar)
		r.Post("/orcamentos/{id}/recusar", h.recusar)
	})
	return r
}

type handlers struct{ uc casosDeUso }

type itemJSON struct {
	Tipo          string `json:"tipo,omitempty"`
	Nome          string `json:"nome"`
	Quantidade    int    `json:"quantidade"`
	PrecoUnitario string `json:"preco_unitario"`
	Subtotal      string `json:"subtotal"`
}

type pagamentoJSON struct {
	ID            uuid.UUID `json:"id"`
	Status        string    `json:"status"`
	LinkPagamento string    `json:"link_pagamento"`
	Valor         string    `json:"valor"`
	MPOrderID     string    `json:"mp_order_id"`
}

type orcamentoJSON struct {
	ID          uuid.UUID      `json:"id"`
	OsID        uuid.UUID      `json:"os_id"`
	NumeroOS    string         `json:"numero_os,omitempty"`
	Status      string         `json:"status"`
	Diagnostico string         `json:"diagnostico,omitempty"`
	Itens       []itemJSON     `json:"itens"`
	ValorTotal  string         `json:"valor_total"`
	Motivo      string         `json:"motivo,omitempty"`
	Pagamento   *pagamentoJSON `json:"pagamento,omitempty"`
	CriadoEm    time.Time      `json:"criado_em"`
}

func paraJSON(o orcamento.Orcamento, p *orcamento.Pagamento) orcamentoJSON {
	out := orcamentoJSON{ID: o.ID, OsID: o.OsID, NumeroOS: o.NumeroOS, Status: string(o.Status), Diagnostico: o.Diagnostico,
		ValorTotal: o.Total.String(), Motivo: o.Motivo, CriadoEm: o.CriadoEm, Itens: []itemJSON{}}
	for _, i := range o.Itens {
		out.Itens = append(out.Itens, itemJSON{Tipo: i.Tipo, Nome: i.Nome, Quantidade: i.Quantidade,
			PrecoUnitario: i.PrecoUnitario.String(), Subtotal: i.Subtotal().String()})
	}
	if p != nil {
		out.Pagamento = &pagamentoJSON{ID: p.ID, Status: string(p.Status), LinkPagamento: p.CheckoutURL, Valor: p.Valor.String(), MPOrderID: p.MPOrderID}
	}
	return out
}

func idDoCaminho(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		erroJSON(w, http.StatusBadRequest, "VALIDATION_ERROR", "id do orçamento inválido")
		return uuid.Nil, false
	}
	return id, true
}

func (h handlers) consultar(w http.ResponseWriter, r *http.Request) {
	id, ok := idDoCaminho(w, r)
	if !ok {
		return
	}
	o, p, err := h.uc.Consultar(r.Context(), id, ator(r.Context()))
	if err != nil {
		responderErro(w, r, err)
		return
	}
	escreverJSON(w, http.StatusOK, paraJSON(o, p))
}

func (h handlers) porOS(w http.ResponseWriter, r *http.Request) {
	osID, err := uuid.Parse(r.URL.Query().Get("os_id"))
	if err != nil {
		erroJSON(w, http.StatusBadRequest, "VALIDATION_ERROR", "os_id obrigatório (uuid)")
		return
	}
	o, p, err := h.uc.ConsultarPorOS(r.Context(), osID, ator(r.Context()))
	if err != nil {
		responderErro(w, r, err)
		return
	}
	escreverJSON(w, http.StatusOK, paraJSON(o, p))
}

func (h handlers) aprovar(w http.ResponseWriter, r *http.Request) {
	id, ok := idDoCaminho(w, r)
	if !ok {
		return
	}
	o, p, err := h.uc.Aprovar(r.Context(), id, ator(r.Context()))
	if err != nil {
		responderErro(w, r, err)
		return
	}
	escreverJSON(w, http.StatusOK, paraJSON(o, &p))
}

func (h handlers) recusar(w http.ResponseWriter, r *http.Request) {
	id, ok := idDoCaminho(w, r)
	if !ok {
		return
	}
	var corpo struct {
		Motivo string `json:"motivo"`
	}
	_ = json.NewDecoder(r.Body).Decode(&corpo) // motivo é opcional
	o, err := h.uc.Recusar(r.Context(), id, ator(r.Context()), corpo.Motivo)
	if err != nil {
		responderErro(w, r, err)
		return
	}
	escreverJSON(w, http.StatusOK, paraJSON(o, nil))
}

// webhook do Mercado Pago (F4-2.4). Ordem: valida a assinatura → consulta a
// order e aplica (cobranca.ConfirmarCobranca) → 200. Se a consulta falhar,
// 500 e nada gravado, para o reenvio do MP ser processado de verdade.
func webhook(uc casosDeUso, segredo string, agora func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		dataID, tipo := q.Get("data.id"), q.Get("type")
		if !mercadopago.AssinaturaValida(segredo, r.Header.Get("x-signature"), r.Header.Get("x-request-id"), dataID, agora()) {
			erroJSON(w, http.StatusUnauthorized, "assinatura_invalida", "x-signature inválido")
			return
		}
		log := logging.Log(r.Context()).With("x_request_id", r.Header.Get("x-request-id"), "data_id", dataID, "tipo", tipo)
		if tipo != "order" {
			log.Info("webhook ignorado: tipo não tratado")
			w.WriteHeader(http.StatusOK)
			return
		}
		err := uc.ConfirmarCobranca(r.Context(), dataID)
		switch {
		case err == nil:
			log.Info("webhook processado")
		case errors.Is(err, cobranca.ErrNaoEncontrado):
			log.Warn("webhook de order desconhecida (ex.: simulação do painel)")
		default:
			log.Error("webhook: falha ao confirmar a cobrança; o MP vai reenviar", "error", err)
			erroJSON(w, http.StatusInternalServerError, "ERRO_INTERNO", "tente de novo")
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func responderErro(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, cobranca.ErrNaoEncontrado):
		erroJSON(w, http.StatusNotFound, "NOT_FOUND", "orçamento não encontrado")
	case errors.Is(err, cobranca.ErrProibido):
		erroJSON(w, http.StatusForbidden, "ACCESS_DENIED", err.Error())
	case errors.Is(err, orcamento.ErrEstadoInvalido), errors.Is(err, cobranca.ErrConflito):
		erroJSON(w, http.StatusConflict, "CONFLITO", err.Error())
	default:
		logging.Log(r.Context()).Error("erro inesperado", "error", err)
		erroJSON(w, http.StatusBadGateway, "ERRO_DEPENDENCIA", "não foi possível concluir; tente de novo")
	}
}
