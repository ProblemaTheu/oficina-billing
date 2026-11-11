package database

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// migrationsFS embute todos os arquivos .sql do diretório migrations no binário
// compilado, eliminando a necessidade de distribuir arquivos externos em produção.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations aplica todas as migrations pendentes no banco de dados usando
// golang-migrate com source iofs (embedded filesystem) e driver postgres.
//
// Comportamento:
//   - Aplica as migrations em ordem crescente de versão (*.up.sql).
//   - Se não houver migrations pendentes (migrate.ErrNoChange), retorna nil —
//     não é tratado como erro.
//   - Qualquer outro erro interrompe a execução e é retornado encapsulado.
//
// A tabela de controle "schema_migrations" é criada automaticamente pelo
// golang-migrate na primeira execução.
func RunMigrations(db *sql.DB) error {
	slog.Info("iniciando execução de migrations")

	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrations: falha ao carregar fonte embedded: %w", err)
	}

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("migrations: falha ao inicializar driver postgres: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return fmt.Errorf("migrations: falha ao criar instância do migrator: %w", err)
	}

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			slog.Info("migrations: schema já está atualizado, nenhuma alteração aplicada")
			return nil
		}
		return fmt.Errorf("migrations: falha ao aplicar migrations pendentes: %w", err)
	}

	version, dirty, _ := m.Version()
	slog.Info("migrations aplicadas com sucesso", "versao_atual", version, "dirty", dirty)

	return nil
}
