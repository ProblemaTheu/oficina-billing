// Package comandos liga os comandos de billing-commands.fifo aos casos de uso.
package comandos

import (
	"context"
	"encoding/json"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/application/logging"
	"github.com/ProblemaTheu/oficina-billing/internal/infra/mensageria"
)

// casosDeUso é o que os handlers usam do cobranca.Servico.
type casosDeUso interface {
	GerarOrcamento(ctx context.Context, cmd cobranca.Comando, p cobranca.PayloadGerarOrcamento) error
	EstornarPagamento(ctx context.Context, cmd cobranca.Comando) error
	CancelarOrcamento(ctx context.Context, cmd cobranca.Comando, motivo string) error
}

func comando(env mensageria.Envelope) cobranca.Comando {
	return cobranca.Comando{MessageID: env.MessageID, Tipo: env.Tipo, SagaID: env.SagaID, OsID: env.OsID}
}

func motivo(env mensageria.Envelope) string {
	var p struct {
		Motivo string `json:"motivo"`
	}
	_ = json.Unmarshal(env.Payload, &p)
	return p.Motivo
}

// Handlers devolve os handlers por tipo de comando.
func Handlers(uc casosDeUso) map[string]mensageria.Handler {
	return map[string]mensageria.Handler{
		cobranca.TipoGerarOrcamento: func(ctx context.Context, env mensageria.Envelope) error {
			var p cobranca.PayloadGerarOrcamento
			if err := env.LerPayload(&p); err != nil {
				// Payload que não desserializa segue vazio: o caso de uso responde
				// OrcamentoFalhou (sem itens) e a saga compensa.
				logging.Log(ctx).Error("payload de GerarOrcamento inválido", "error", err)
				p = cobranca.PayloadGerarOrcamento{}
			}
			return uc.GerarOrcamento(ctx, comando(env), p)
		},
		cobranca.TipoEstornarPagamento: func(ctx context.Context, env mensageria.Envelope) error {
			return uc.EstornarPagamento(ctx, comando(env))
		},
		cobranca.TipoCancelarOrcamento: func(ctx context.Context, env mensageria.Envelope) error {
			return uc.CancelarOrcamento(ctx, comando(env), motivo(env))
		},
	}
}
