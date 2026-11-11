package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// InboxRepository torna o consumo idempotente (F4-4.3): o handler marca a
// mensagem na mesma transação do efeito e, se ela já estava marcada, faz
// commit sem efeito e dá ack.
type InboxRepository struct{}

// NovoInboxRepository cria o repositório.
func NovoInboxRepository() *InboxRepository { return &InboxRepository{} }

// Marcar registra o message_id. nova=false significa duplicata: o efeito já
// foi aplicado numa entrega anterior.
func (InboxRepository) Marcar(ctx context.Context, messageID uuid.UUID, tipo string) (bool, error) {
	tx, err := txObrigatoria(ctx)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO mensagens_processadas (message_id, tipo) VALUES ($1, $2)
		ON CONFLICT (message_id) DO NOTHING`, messageID, tipo)
	if err != nil {
		return false, fmt.Errorf("InboxRepository.Marcar: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("InboxRepository.Marcar rows: %w", err)
	}
	return n == 1, nil
}
