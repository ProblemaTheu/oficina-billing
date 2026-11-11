package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/ProblemaTheu/oficina-billing/internal/application/cobranca"
	"github.com/ProblemaTheu/oficina-billing/internal/domain/orcamento"
)

// PagamentoRepository persiste as cobranças do Mercado Pago.
type PagamentoRepository struct{ db *sql.DB }

var _ cobranca.Pagamentos = (*PagamentoRepository)(nil)

// NovoPagamentoRepository cria o repositório.
func NovoPagamentoRepository(db *sql.DB) *PagamentoRepository { return &PagamentoRepository{db: db} }

// Criar grava a cobrança na transação do contexto.
func (r *PagamentoRepository) Criar(ctx context.Context, p orcamento.Pagamento) error {
	tx, err := txObrigatoria(ctx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO pagamentos (id, orcamento_id, mp_order_id, checkout_url, status, valor_centavos, criado_em, atualizado_em)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		p.ID, p.OrcamentoID, p.MPOrderID, p.CheckoutURL, string(p.Status), int64(p.Valor), p.CriadoEm, p.AtualizadoEm)
	if err != nil {
		return fmt.Errorf("PagamentoRepository.Criar: %w", err)
	}
	return nil
}

// Queries literais, uma por filtro (sem concatenação de SQL).
const (
	pagamentoPorOrcamento = `SELECT id, orcamento_id, mp_order_id, checkout_url, status, valor_centavos,
		coalesce(status_detail,''), coalesce(mp_refund_id,''), criado_em, atualizado_em
		FROM pagamentos WHERE orcamento_id = $1`
	pagamentoPorOrder = `SELECT id, orcamento_id, mp_order_id, checkout_url, status, valor_centavos,
		coalesce(status_detail,''), coalesce(mp_refund_id,''), criado_em, atualizado_em
		FROM pagamentos WHERE mp_order_id = $1`
	pagamentosPendentes = `SELECT id, orcamento_id, mp_order_id, checkout_url, status, valor_centavos,
		coalesce(status_detail,''), coalesce(mp_refund_id,''), criado_em, atualizado_em
		FROM pagamentos WHERE status = 'pendente' ORDER BY criado_em LIMIT $1`
)

func lerPagamento(sc interface{ Scan(...any) error }) (orcamento.Pagamento, error) {
	var (
		p      orcamento.Pagamento
		status string
		valor  int64
	)
	err := sc.Scan(&p.ID, &p.OrcamentoID, &p.MPOrderID, &p.CheckoutURL, &status, &valor, &p.StatusDetail, &p.MPRefundID, &p.CriadoEm, &p.AtualizadoEm)
	p.Status, p.Valor = orcamento.StatusPagamento(status), orcamento.Centavos(valor)
	return p, err
}

func (r *PagamentoRepository) buscar(ctx context.Context, query string, arg any) (orcamento.Pagamento, error) {
	p, err := lerPagamento(execDe(ctx, r.db).QueryRowContext(ctx, query, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return p, cobranca.ErrNaoEncontrado
	}
	if err != nil {
		return p, fmt.Errorf("PagamentoRepository.buscar: %w", err)
	}
	return p, nil
}

// BuscarPorOrcamento: a cobrança (única) do orçamento.
func (r *PagamentoRepository) BuscarPorOrcamento(ctx context.Context, orcamentoID uuid.UUID) (orcamento.Pagamento, error) {
	return r.buscar(ctx, pagamentoPorOrcamento, orcamentoID)
}

// BuscarPorOrder: a cobrança de uma order do Mercado Pago.
func (r *PagamentoRepository) BuscarPorOrder(ctx context.Context, orderID string) (orcamento.Pagamento, error) {
	return r.buscar(ctx, pagamentoPorOrder, orderID)
}

// MudarStatus grava a transição se o status ainda for `anterior` — é o que
// impede webhook e reconciliador de confirmarem o mesmo pagamento duas vezes.
func (r *PagamentoRepository) MudarStatus(ctx context.Context, p orcamento.Pagamento, anterior orcamento.StatusPagamento) error {
	res, err := execDe(ctx, r.db).ExecContext(ctx, `
		UPDATE pagamentos SET status = $1, status_detail = $2, mp_refund_id = $3, atualizado_em = $4
		WHERE id = $5 AND status = $6`,
		string(p.Status), p.StatusDetail, p.MPRefundID, p.AtualizadoEm, p.ID, string(anterior))
	if err != nil {
		return fmt.Errorf("PagamentoRepository.MudarStatus: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return cobranca.ErrConflito
	}
	return nil
}

// Pendentes devolve as cobranças aguardando confirmação, das mais antigas.
func (r *PagamentoRepository) Pendentes(ctx context.Context, limite int) ([]orcamento.Pagamento, error) {
	rows, err := r.db.QueryContext(ctx, pagamentosPendentes, limite)
	if err != nil {
		return nil, fmt.Errorf("PagamentoRepository.Pendentes: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []orcamento.Pagamento
	for rows.Next() {
		p, err := lerPagamento(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
