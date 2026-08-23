package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"papo/internal/utils"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var db *sql.DB

// ErrNotFound indica que o registro não foi encontrado.
var ErrNotFound = errors.New("registro não encontrado")

// ErrUniqueViolation indica que uma constraint UNIQUE foi violada.
var ErrUniqueViolation = errors.New("violação de constraint UNIQUE")

// rowScanner é implementado por sql.Row e sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// mapStorageError normaliza erros comuns do PostgreSQL em erros do storage.
func mapStorageError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrUniqueViolation
	}

	return err
}

// InitDB inicializa a conexão com o PostgreSQL
func InitDB(databaseURL string) error {
	var err error

	db, err = sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("falha ao abrir conexão com PostgreSQL: %w", err)
	}

	db.SetMaxIdleConns(10)
	db.SetMaxOpenConns(100)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("falha ao pingar PostgreSQL: %w", err)
	}

	utils.Info("Conexão com PostgreSQL estabelecida com sucesso")
	return nil
}

// GetDB retorna a conexão com o PostgreSQL
func GetDB() *sql.DB {
	return db
}

// CloseDB fecha a conexão com o PostgreSQL
func CloseDB() error {
	if db == nil {
		return nil
	}

	return db.Close()
}
