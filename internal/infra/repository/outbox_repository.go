package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ProblemaTheu/oficina-billing/internal/infra/mensageria"
	"github.com/google/uuid"
)

// chaveLiderancaRelay identifica o advisory lock do relay da outbox. Cada
// serviço tem banco próprio, então a mesma constante nos 3 repos não colide.
const chaveLiderancaRelay int64 = 4_000_000_001

// OutboxRepository implementa a outbox transacional em PostgreSQL (F4-4.2):
// Gravar dentro da transação do efeito e mensageria.OutboxStore para o relay.
type OutboxRepository struct {
	db *sql.DB
}

var _ mensageria.OutboxStore = (*OutboxRepository)(nil)

// NovoOutboxRepository cria o repositório.
func NovoOutboxRepository(db *sql.DB) *OutboxRepository { return &OutboxRepository{db: db} }

// Gravar insere a mensagem na outbox dentro da transação do contexto. Os
// headers de trace são capturados aqui, enquanto a transação do New Relic que
// originou a mensagem ainda existe.
func (r *OutboxRepository) Gravar(ctx context.Context, destino string, env mensageria.Envelope, trace map[string]string) error {
	tx, err := txObrigatoria(ctx)
	if err != nil {
		return err
	}
	if err := env.Validar(); err != nil {
		return err
	}
	corpo, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("OutboxRepository.Gravar envelope: %w", err)
	}
	if trace == nil {
		trace = map[string]string{}
	}
	headers, err := json.Marshal(trace)
	if err != nil {
		return fmt.Errorf("OutboxRepository.Gravar trace: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox (id, destino, tipo, envelope, trace_headers)
		VALUES ($1, $2, $3, $4, $5)`, uuid.New(), destino, env.Tipo, corpo, headers); err != nil {
		return fmt.Errorf("OutboxRepository.Gravar: %w", err)
	}
	return nil
}

// TentarLideranca pega o advisory lock de sessão numa conexão dedicada. A
// conexão fica presa até liberar() — se o pod morrer, o Postgres solta o lock
// ao fechar a sessão.
func (r *OutboxRepository) TentarLideranca(ctx context.Context) (func(), bool, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var lider bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, chaveLiderancaRelay).Scan(&lider); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	if !lider {
		_ = conn.Close()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, chaveLiderancaRelay)
		_ = conn.Close()
	}, true, nil
}

// Pendentes devolve as mensagens não enviadas e não desistidas, na ordem de gravação.
func (r *OutboxRepository) Pendentes(ctx context.Context, limite int) ([]mensageria.MensagemOutbox, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, destino, envelope, trace_headers, tentativas
		FROM outbox
		WHERE enviado_em IS NULL AND falhou_em IS NULL
		ORDER BY seq
		LIMIT $1`, limite)
	if err != nil {
		return nil, fmt.Errorf("OutboxRepository.Pendentes: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []mensageria.MensagemOutbox
	for rows.Next() {
		var (
			m             mensageria.MensagemOutbox
			corpo, header []byte
		)
		if err := rows.Scan(&m.ID, &m.Destino, &corpo, &header, &m.Tentativas); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(corpo, &m.Envelope); err != nil {
			return nil, fmt.Errorf("OutboxRepository.Pendentes envelope %s: %w", m.ID, err)
		}
		if err := json.Unmarshal(header, &m.Trace); err != nil {
			return nil, fmt.Errorf("OutboxRepository.Pendentes trace %s: %w", m.ID, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarcarEnviada registra o envio.
func (r *OutboxRepository) MarcarEnviada(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `UPDATE outbox SET enviado_em = now() WHERE id = $1`, id)
	return err
}

// RegistrarFalha incrementa as tentativas e, com desistir, tira a linha da fila do relay.
func (r *OutboxRepository) RegistrarFalha(ctx context.Context, id uuid.UUID, desistir bool) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE outbox
		SET tentativas = tentativas + 1,
		    falhou_em  = CASE WHEN $2 THEN now() ELSE falhou_em END
		WHERE id = $1`, id, desistir)
	return err
}

// ContarPendentes é a métrica de saúde da outbox: crescendo = SQS inacessível.
func (r *OutboxRepository) ContarPendentes(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox WHERE enviado_em IS NULL AND falhou_em IS NULL`).Scan(&n)
	return n, err
}

// LimparEnviadas apaga as mensagens enviadas há mais de idade (rotina diária).
func (r *OutboxRepository) LimparEnviadas(ctx context.Context, idade time.Duration) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM outbox WHERE enviado_em < now() - make_interval(secs => $1)`, idade.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
