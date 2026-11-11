package repository

import (
	"context"
	"time"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/application/logging"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/mensageria"
)

// DestinoOS é o destino lógico das respostas (os-saga-replies.fifo).
const DestinoOS = "os"

// Respostas implementa cobranca.Respostas sobre a outbox: monta o envelope
// (origem billing-service, correlation_id do contexto) e grava na transação.
type Respostas struct {
	outbox *OutboxRepository
	tracer mensageria.Tracer
}

var _ cobranca.Respostas = (*Respostas)(nil)

// NovasRespostas cria o adaptador. tracer nil = sem propagação de trace.
func NovasRespostas(outbox *OutboxRepository, tracer mensageria.Tracer) *Respostas {
	if tracer == nil {
		tracer = mensageria.TracerNulo{}
	}
	return &Respostas{outbox: outbox, tracer: tracer}
}

// Responder grava a resposta na outbox.
func (r *Respostas) Responder(ctx context.Context, s cobranca.Saida) error {
	env, err := mensageria.Novo(s.Tipo, s.SagaID, s.OsID, logging.Correlacao(ctx), mensageria.OrigemBilling, s.Payload, time.Now())
	if err != nil {
		return err
	}
	return r.outbox.Gravar(ctx, DestinoOS, env, r.tracer.Capturar(ctx))
}
