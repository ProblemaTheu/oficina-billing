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

// OrcamentoRepository persiste orçamentos e itens.
type OrcamentoRepository struct{ db *sql.DB }

var _ cobranca.Orcamentos = (*OrcamentoRepository)(nil)

// NovoOrcamentoRepository cria o repositório.
func NovoOrcamentoRepository(db *sql.DB) *OrcamentoRepository { return &OrcamentoRepository{db: db} }

func nulo(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// Criar grava o orçamento e os itens na transação do contexto.
func (r *OrcamentoRepository) Criar(ctx context.Context, o orcamento.Orcamento) error {
	tx, err := txObrigatoria(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO orcamentos (id, os_id, saga_id, numero_os, cliente_id, cliente_nome, cliente_email, status,
		                        diagnostico, valor_total_centavos, motivo, criado_em, atualizado_em)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		o.ID, o.OsID, o.SagaID, o.NumeroOS, nulo(o.Cliente.ID), o.Cliente.Nome, o.Cliente.Email, string(o.Status),
		o.Diagnostico, int64(o.Total), o.Motivo, o.CriadoEm, o.AtualizadoEm); err != nil {
		return fmt.Errorf("OrcamentoRepository.Criar: %w", err)
	}
	for n, i := range o.Itens {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO itens_orcamento (id, orcamento_id, ordem, tipo, referencia, nome, quantidade, preco_unitario_centavos)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			uuid.New(), o.ID, n, i.Tipo, i.Ref, i.Nome, i.Quantidade, int64(i.PrecoUnitario)); err != nil {
			return fmt.Errorf("OrcamentoRepository.Criar item: %w", err)
		}
	}
	return nil
}

// Queries literais, uma por filtro: nada de SQL montado por concatenação
// (gosec G202 / Sonar — foi o que derrubou o quality gate no oficina-app#22).
const (
	orcamentoPorID = `SELECT id, os_id, saga_id, coalesce(numero_os,''), cliente_id, coalesce(cliente_nome,''),
		coalesce(cliente_email,''), status, coalesce(diagnostico,''), valor_total_centavos, coalesce(motivo,''), criado_em, atualizado_em
		FROM orcamentos WHERE id = $1`
	orcamentoPorOS = `SELECT id, os_id, saga_id, coalesce(numero_os,''), cliente_id, coalesce(cliente_nome,''),
		coalesce(cliente_email,''), status, coalesce(diagnostico,''), valor_total_centavos, coalesce(motivo,''), criado_em, atualizado_em
		FROM orcamentos WHERE os_id = $1`
)

func (r *OrcamentoRepository) buscar(ctx context.Context, query string, arg any) (orcamento.Orcamento, error) {
	var (
		o       orcamento.Orcamento
		cliente uuid.NullUUID
		status  string
		total   int64
	)
	ex := execDe(ctx, r.db)
	err := ex.QueryRowContext(ctx, query, arg).
		Scan(&o.ID, &o.OsID, &o.SagaID, &o.NumeroOS, &cliente, &o.Cliente.Nome, &o.Cliente.Email, &status,
			&o.Diagnostico, &total, &o.Motivo, &o.CriadoEm, &o.AtualizadoEm)
	if errors.Is(err, sql.ErrNoRows) {
		return o, cobranca.ErrNaoEncontrado
	}
	if err != nil {
		return o, fmt.Errorf("OrcamentoRepository.buscar: %w", err)
	}
	o.Status, o.Total = orcamento.Status(status), orcamento.Centavos(total)
	if cliente.Valid {
		o.Cliente.ID = cliente.UUID
	}
	rows, err := ex.QueryContext(ctx, `
		SELECT coalesce(tipo,''), coalesce(referencia,''), nome, quantidade, preco_unitario_centavos
		FROM itens_orcamento WHERE orcamento_id = $1 ORDER BY ordem`, o.ID)
	if err != nil {
		return o, fmt.Errorf("OrcamentoRepository.buscar itens: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var (
			i     orcamento.Item
			preco int64
		)
		if err := rows.Scan(&i.Tipo, &i.Ref, &i.Nome, &i.Quantidade, &preco); err != nil {
			return o, err
		}
		i.PrecoUnitario = orcamento.Centavos(preco)
		o.Itens = append(o.Itens, i)
	}
	return o, rows.Err()
}

// Buscar por id.
func (r *OrcamentoRepository) Buscar(ctx context.Context, id uuid.UUID) (orcamento.Orcamento, error) {
	return r.buscar(ctx, orcamentoPorID, id)
}

// BuscarPorOS: o orçamento (único) da OS.
func (r *OrcamentoRepository) BuscarPorOS(ctx context.Context, osID uuid.UUID) (orcamento.Orcamento, error) {
	return r.buscar(ctx, orcamentoPorOS, osID)
}

// MudarStatus grava status e motivo se o status ainda for `anterior`.
func (r *OrcamentoRepository) MudarStatus(ctx context.Context, o orcamento.Orcamento, anterior orcamento.Status) error {
	res, err := execDe(ctx, r.db).ExecContext(ctx, `
		UPDATE orcamentos SET status = $1, motivo = $2, atualizado_em = $3 WHERE id = $4 AND status = $5`,
		string(o.Status), o.Motivo, o.AtualizadoEm, o.ID, string(anterior))
	if err != nil {
		return fmt.Errorf("OrcamentoRepository.MudarStatus: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return cobranca.ErrConflito
	}
	return nil
}
