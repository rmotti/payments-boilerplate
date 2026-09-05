// Package database creates the shared PostgreSQL connection and GORM adapter.
package database

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx as a database/sql driver.
	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/plugin/opentelemetry/tracing"
)

// Database exposes both GORM and database/sql over one pool.
type Database struct {
	GORM *gorm.DB
	SQL  *sql.DB
}

// Open creates and verifies a PostgreSQL connection pool.
func Open(ctx context.Context, cfg config.Config, log *zap.Logger) (*Database, error) {
	sqlDB, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres driver: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.DatabaseMaxOpenConnections)
	sqlDB.SetMaxIdleConns(cfg.DatabaseMaxIdleConnections)
	sqlDB.SetConnMaxLifetime(cfg.DatabaseConnectionMaxLifetime)

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		DisableAutomaticPing:                     true,
		DisableForeignKeyConstraintWhenMigrating: true,
		Logger:                                   logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initialize gorm: %w", err)
	}
	if err := gormDB.Use(tracing.NewPlugin(tracing.WithoutQueryVariables())); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("enable gorm telemetry: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	log.Info("postgres connection established")
	return &Database{GORM: gormDB, SQL: sqlDB}, nil
}

// Ping verifies that PostgreSQL is reachable.
func (db *Database) Ping(ctx context.Context) error {
	return db.SQL.PingContext(ctx)
}

// Close releases the PostgreSQL pool.
func (db *Database) Close() error {
	return db.SQL.Close()
}
