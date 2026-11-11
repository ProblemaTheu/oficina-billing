package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrSemTransacao indica que uma operação que só faz sentido dentro de uma
// transação (reserva de estoque, outbox, inbox) foi chamada fora de uma.
var ErrSemTransacao = errors.New("operação exige transação no contexto (UnitOfWork.Executar)")

type ctxTx struct{}

// ComTx devolve um contexto carregando a transação. Os repositórios que a
// encontram no contexto a usam em vez de abrir conexão própria.
func ComTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, ctxTx{}, tx)
}

func txDe(ctx context.Context) (*sql.Tx, bool) {
	tx, ok := ctx.Value(ctxTx{}).(*sql.Tx)
	return tx, ok && tx != nil
}

func txObrigatoria(ctx context.Context) (*sql.Tx, error) {
	if tx, ok := txDe(ctx); ok {
		return tx, nil
	}
	return nil, ErrSemTransacao
}

// UnitOfWork implementa a porta de unidade de trabalho do caso de uso: tudo o
// que roda dentro de Executar compartilha uma transação — é o que torna OS,
// reserva, saga e outbox atômicas (F4-1.3). A transação viaja no contexto, de
// modo que a camada de aplicação não conhece *sql.Tx.
type UnitOfWork struct {
	db *sql.DB
}

// NovaUnitOfWork cria a unidade de trabalho sobre o pool.
func NovaUnitOfWork(db *sql.DB) *UnitOfWork { return &UnitOfWork{db: db} }

// Executar roda fn numa transação: commit se fn devolver nil, rollback caso
// contrário. Chamado dentro de outra unidade, reaproveita a transação externa.
func (u *UnitOfWork) Executar(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := txDe(ctx); ok {
		return fn(ctx)
	}
	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("UnitOfWork begin: %w", err)
	}
	if err := fn(ComTx(ctx, tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("UnitOfWork commit: %w", err)
	}
	return nil
}

// executor é o que *sql.DB e *sql.Tx têm em comum.
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// execDe devolve a transação do contexto, se houver, ou o pool.
func execDe(ctx context.Context, db *sql.DB) executor {
	if tx, ok := txDe(ctx); ok {
		return tx
	}
	return db
}
