// Package database provê a conexão com o banco de dados PostgreSQL e
// utilitários de configuração lidos de variáveis de ambiente.
//
// Variáveis de ambiente reconhecidas:
//   - DB_HOST     (padrão: "localhost")
//   - DB_PORT     (padrão: "5432")
//   - DB_USER     (padrão: "postgres")
//   - DB_PASSWORD (padrão: "admin")
//   - DB_NAME     (padrão: "tech_challenge_db")
package database

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	_ "github.com/lib/pq"
)

// Connect abre e valida uma conexão com o banco de dados PostgreSQL usando as
// variáveis de ambiente de configuração.
//
// A função chama log.Fatal se não conseguir abrir o driver, pois sem banco de
// dados a aplicação não tem condições de operar.
//
// Se o Ping falhar (banco temporariamente indisponível), a conexão é retornada
// assim mesmo — o pool do database/sql tentará reconectar automaticamente nas
// requisições seguintes.
func Connect() *sql.DB {
	host := getEnv("DB_HOST", "localhost")
	port := getEnv("DB_PORT", "5432")
	user := getEnv("DB_USER", "postgres")
	password := getEnv("DB_PASSWORD", "postgres")
	dbname := getEnv("DB_NAME", "billing")
	// O RDS PostgreSQL 15 vem com rds.force_ssl=1: conexão sem TLS é recusada
	// com "no pg_hba.conf entry ... no encryption", que parece erro de
	// permissão e não é. Local (docker compose e kind) segue em disable.
	sslmode := getEnv("DB_SSLMODE", "disable")

	connStr := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		host, port, user, password, dbname, sslmode,
	)

	slog.Info("conectando ao banco de dados", "host", host, "port", port, "dbname", dbname)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		slog.Error("falha ao inicializar driver do banco de dados", "error", err)
		os.Exit(1)
	}

	if err := db.Ping(); err != nil {
		slog.Warn("banco de dados não respondeu ao ping — a aplicação continuará tentando reconectar", "error", err)
		return db
	}

	slog.Info("conexão com o banco de dados estabelecida com sucesso", "host", host, "port", port, "dbname", dbname)
	return db
}

// getEnv retorna o valor da variável de ambiente key, ou fallback se ela não
// estiver definida ou estiver vazia.
func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
