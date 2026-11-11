package cobranca

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ProblemaTheu/oficina-billing/internal/application/logging"
	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
)

// ── Confirmação do pagamento (webhook e reconciliador) ───────────────────────

// Status de order do Mercado Pago (API de Orders) — validados no sandbox em
// 10/11 (processed, action_required/waiting_retry, canceled, refunded).
const (
	mpProcessado     = "processed"
	mpAcaoNecessaria = "action_required" // tentativa recusada; a order aceita nova tentativa
	mpFalhou         = "failed"
	mpCancelado      = "canceled"
	mpExpirado       = "expired"
	mpEstornado      = "refunded"
)

// ConfirmarCobranca consulta a order e aplica o estado — é chamada pelo webhook
// (acelerador) e pelo reconciliador (mecanismo principal: no sandbox o Mercado
// Pago não notifica orders de teste). A verdade vem do GET, nunca do corpo do
// webhook. Só age sobre pagamentos pendentes, então webhook e reconciliador
// processando a mesma order ao mesmo tempo não duplicam efeito.
func (s *Servico) ConfirmarCobranca(ctx context.Context, orderID string) error {
	p, err := s.d.Pagamentos.BuscarPorOrder(ctx, orderID)
	if err != nil {
		return err
	}
	if p.Status != orcamento.PagamentoPendente {
		return nil
	}
	estado, err := s.d.Gateway.Consultar(ctx, orderID)
	if err != nil {
		return fmt.Errorf("consultar order %s: %w", orderID, err)
	}
	o, err := s.d.Orcamentos.Buscar(ctx, p.OrcamentoID)
	if err != nil {
		return err
	}
	log := logging.Log(ctx).With("mp_order_id", orderID, "orcamento_id", o.ID, "mp_status", estado.Status)

	switch estado.Status {
	case mpProcessado:
		if o.Status == orcamento.Cancelado {
			// Pagou depois de a saga compensar: devolve e não confirma (F4-2.5 passo 4).
			log.Warn("pagamento após o cancelamento do orçamento; estornando")
			return s.estornar(ctx, o, p, "pagamento-tardio-"+p.ID.String(), nil)
		}
		return s.mudarPagamento(ctx, p, orcamento.PagamentoAprovado, estado.StatusDetail, &Saida{
			Tipo: TipoPagamentoConfirmado, SagaID: o.SagaID, OsID: o.OsID,
			Payload: map[string]string{"pagamento_id": p.ID.String(), "mp_order_id": orderID, "valor": estado.Pago.String()},
		})
	case mpAcaoNecessaria:
		// Uma tentativa foi recusada, mas a order continua pagável (até 20
		// tentativas; retries.allowed=false é ignorado pelo Checkout Pro).
		// Cancelar ANTES de recusar: sem isso o cliente paga depois que a saga
		// compensou. Se o cancelamento falhar porque ele já pagou numa nova
		// tentativa, o próximo ciclo encontra a order processada.
		if err := s.d.Gateway.Cancelar(ctx, orderID); err != nil {
			return fmt.Errorf("cancelar order recusada %s: %w", orderID, err)
		}
		return s.recusar(ctx, o, p, estado.StatusDetail)
	case mpFalhou, mpCancelado, mpExpirado:
		return s.recusar(ctx, o, p, estado.StatusDetail)
	case mpEstornado:
		return s.mudarPagamento(ctx, p, orcamento.PagamentoEstornado, estado.StatusDetail, nil)
	default: // created, processing: aguarda o próximo ciclo
		return nil
	}
}

func (s *Servico) recusar(ctx context.Context, o orcamento.Orcamento, p orcamento.Pagamento, detalhe string) error {
	if detalhe == "" {
		detalhe = "pagamento_recusado"
	}
	return s.mudarPagamento(ctx, p, orcamento.PagamentoRecusado, detalhe, &Saida{
		Tipo: TipoPagamentoRecusado, SagaID: o.SagaID, OsID: o.OsID,
		Payload: map[string]string{"orcamento_id": o.ID.String(), "mp_status_detail": detalhe},
	})
}

// mudarPagamento grava a transição (condicionada a pendente) e a resposta.
// Conflito = outro processo já aplicou: sem erro, sem efeito.
func (s *Servico) mudarPagamento(ctx context.Context, p orcamento.Pagamento, para orcamento.StatusPagamento, detalhe string, saida *Saida) error {
	anterior := p.Status
	p.Status, p.StatusDetail, p.AtualizadoEm = para, detalhe, s.agora().UTC()
	err := s.d.UoW.Executar(ctx, func(ctx context.Context) error {
		if err := s.d.Pagamentos.MudarStatus(ctx, p, anterior); err != nil {
			return err
		}
		if saida == nil {
			return nil
		}
		return s.d.Respostas.Responder(ctx, *saida)
	})
	if errors.Is(err, ErrConflito) {
		return nil
	}
	return err
}

// Reconciliar confere as cobranças pendentes (mecanismo principal de
// confirmação). Um erro numa order não impede as demais.
func (s *Servico) Reconciliar(ctx context.Context) (int, error) {
	pendentes, err := s.d.Pagamentos.Pendentes(ctx, 50)
	if err != nil {
		return 0, err
	}
	for _, p := range pendentes {
		if err := s.ConfirmarCobranca(ctx, p.MPOrderID); err != nil {
			logging.Log(ctx).Warn("reconciliação de cobrança falhou", "mp_order_id", p.MPOrderID, "error", err)
		}
	}
	return len(pendentes), nil
}

// ── Compensações (comandos da saga) ──────────────────────────────────────────

// EstornarPagamento devolve o pagamento aprovado do orçamento da OS e responde
// PagamentoEstornado. Sem pagamento aprovado (falha antes de pagar, ou já
// estornado), responde mesmo assim — compensar o que não aconteceu é sucesso.
// Erro do Mercado Pago volta para a fila (reentrega → DLQ após 5); a chave de
// idempotência (message_id) evita estorno duplo nas retentativas.
func (s *Servico) EstornarPagamento(ctx context.Context, cmd Comando) error {
	o, err := s.d.Orcamentos.BuscarPorOS(ctx, cmd.OsID)
	if errors.Is(err, ErrNaoEncontrado) {
		return s.responderComando(ctx, cmd, Saida{Tipo: TipoPagamentoEstornado, SagaID: cmd.SagaID, OsID: cmd.OsID,
			Payload: map[string]any{"refund_id": nil}})
	}
	if err != nil {
		return err
	}
	p, err := s.d.Pagamentos.BuscarPorOrcamento(ctx, o.ID)
	if err != nil && !errors.Is(err, ErrNaoEncontrado) {
		return err
	}
	if err != nil || p.Status != orcamento.PagamentoAprovado {
		return s.responderComando(ctx, cmd, Saida{Tipo: TipoPagamentoEstornado, SagaID: o.SagaID, OsID: o.OsID,
			Payload: map[string]any{"refund_id": nil}})
	}
	return s.estornar(ctx, o, p, cmd.MessageID.String(), &cmd)
}

// estornar chama o Mercado Pago e grava o estorno; com cmd, marca a inbox e
// responde PagamentoEstornado à saga.
func (s *Servico) estornar(ctx context.Context, o orcamento.Orcamento, p orcamento.Pagamento, chave string, cmd *Comando) error {
	refund, err := s.d.Gateway.Estornar(ctx, p.MPOrderID, chave)
	if err != nil && !errors.Is(err, ErrJaEstornado) {
		return fmt.Errorf("estornar order %s: %w", p.MPOrderID, err)
	}
	anterior := p.Status
	p.Status, p.MPRefundID, p.AtualizadoEm = orcamento.PagamentoEstornado, refund, s.agora().UTC()
	resposta := Saida{Tipo: TipoPagamentoEstornado, SagaID: o.SagaID, OsID: o.OsID, Payload: map[string]any{"refund_id": refund}}
	err = s.d.UoW.Executar(ctx, func(ctx context.Context) error {
		if cmd != nil {
			if ok, err := s.d.Inbox.Marcar(ctx, cmd.MessageID, cmd.Tipo); err != nil || !ok {
				return err
			}
		}
		if err := s.d.Pagamentos.MudarStatus(ctx, p, anterior); err != nil && !errors.Is(err, ErrConflito) {
			return err
		}
		if cmd == nil {
			return nil // estorno de pagamento tardio: a saga não espera resposta
		}
		return s.d.Respostas.Responder(ctx, resposta)
	})
	if err == nil {
		logging.Log(ctx).Info("pagamento estornado", "orcamento_id", o.ID, "mp_order_id", p.MPOrderID, "refund_id", refund)
	}
	return err
}

// CancelarOrcamento (compensação): cancela o orçamento e, se a cobrança ainda
// estiver pendente, fecha a order no Mercado Pago (o link deixa de aceitar
// pagamento). Responde OrcamentoCancelado mesmo se já estava cancelado.
func (s *Servico) CancelarOrcamento(ctx context.Context, cmd Comando, motivo string) error {
	o, err := s.d.Orcamentos.BuscarPorOS(ctx, cmd.OsID)
	if errors.Is(err, ErrNaoEncontrado) {
		return s.responderComando(ctx, cmd, Saida{Tipo: TipoOrcamentoCancelado, SagaID: cmd.SagaID, OsID: cmd.OsID, Payload: map[string]string{}})
	}
	if err != nil {
		return err
	}
	p, errPag := s.d.Pagamentos.BuscarPorOrcamento(ctx, o.ID)
	if errPag != nil && !errors.Is(errPag, ErrNaoEncontrado) {
		return errPag
	}
	pendente := errPag == nil && p.Status == orcamento.PagamentoPendente
	if pendente {
		if err := s.d.Gateway.Cancelar(ctx, p.MPOrderID); err != nil {
			// Já pago no meio do caminho: a reconciliação encontra a order
			// processada com o orçamento cancelado e estorna (pagamento tardio).
			logging.Log(ctx).Warn("não foi possível cancelar a order; a reconciliação trata", "mp_order_id", p.MPOrderID, "error", err)
		}
	}
	anterior := o.Status
	if err := o.Cancelar(strings.TrimSpace(motivo), s.agora()); err != nil {
		return err
	}
	return s.d.UoW.Executar(ctx, func(ctx context.Context) error {
		if ok, err := s.d.Inbox.Marcar(ctx, cmd.MessageID, cmd.Tipo); err != nil || !ok {
			return err
		}
		if anterior != orcamento.Cancelado {
			if err := s.d.Orcamentos.MudarStatus(ctx, o, anterior); err != nil {
				return err
			}
		}
		if pendente {
			// Tira a cobrança da reconciliação: uma order cancelada por nós
			// não é "pagamento recusado" para a saga.
			anteriorPag := p.Status
			p.Status, p.AtualizadoEm = orcamento.PagamentoCancelado, s.agora().UTC()
			if err := s.d.Pagamentos.MudarStatus(ctx, p, anteriorPag); err != nil && !errors.Is(err, ErrConflito) {
				return err
			}
		}
		return s.d.Respostas.Responder(ctx, Saida{Tipo: TipoOrcamentoCancelado, SagaID: o.SagaID, OsID: o.OsID,
			Payload: map[string]string{"orcamento_id": o.ID.String()}})
	})
}

// responderComando marca a inbox e responde, sem outro efeito.
func (s *Servico) responderComando(ctx context.Context, cmd Comando, r Saida) error {
	return s.d.UoW.Executar(ctx, func(ctx context.Context) error {
		if ok, err := s.d.Inbox.Marcar(ctx, cmd.MessageID, cmd.Tipo); err != nil || !ok {
			return err
		}
		return s.d.Respostas.Responder(ctx, r)
	})
}
