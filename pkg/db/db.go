package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	migrations "dbot/db"
	"dbot/pkg/backup"
	"dbot/pkg/config"
	dbotlog "dbot/pkg/logger"
	"dbot/pkg/store"

	"github.com/pressly/goose/v3"
	"modernc.org/sqlite"
)

func (s db) configure() {
	s.w.SetMaxOpenConns(1)
	s.w.SetConnMaxLifetime(0)
	s.w.SetConnMaxIdleTime(0)

	// s.r.SetMaxOpenConns(100)
	// s.r.SetConnMaxLifetime(5 * time.Minute)
	// s.r.SetMaxIdleConns(2)
}

func ConnectStore(ctx context.Context, filename string, schema string) (*Store, error) {
	slog.Debug("[dupa]", slog.String("filepath.Join(config.DB_DIR, filename)", filepath.Join(config.DB_DIR, filename)))
	w, err := sql.Open("sqlite", filepath.Join(config.DB_DIR, filename))
	if err != nil {
		return nil, err
	}
	// r, err := sql.Open("sqlite", filepath.Join(config.DB_DIR,filename))
	// if err != nil {
	// 	return nil, err
	// }

	go func() {
		<-ctx.Done()
		w.Close()
	}()

	goose.SetBaseFS(migrations.DBmigrations)
	goose.SetDialect("sqlite3")
	err = goose.UpContext(ctx, w, "migrations")
	if err != nil {
		panic(err)
	}

	_, err = w.ExecContext(ctx, "PRAGMA journal_mode=WAL")
	if err != nil {
		panic(err)
	}

	_, err = w.ExecContext(ctx, "PRAGMA synchronous=NORMAL")
	if err != nil {
		panic(err)
	}

	d := db{
		w: w,
	}
	d.configure()
	return &Store{Queries: store.New(d), conn: w}, nil
}

// store.Queries plus the connection, so callers can run a transaction without a
// global db handle.
type Store struct {
	*store.Queries
	conn *sql.DB
}

// fn gets queries bound to the tx, so all of its statements commit or roll back
// together. It must not do network work.
func (s *Store) WithTx(ctx context.Context, fn func(*store.Queries) error) error {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close releases the underlying connection.
func (s *Store) Close() error {
	return s.conn.Close()
}

func ConnectBackup(ctx context.Context, filename, password string) (*backup.Queries, error) {
	w, err := sql.Open("sqlite", filepath.Join(config.BACKUP_DIR, filename))
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		w.Close()
	}()

	goose.SetBaseFS(migrations.BackupMigrations)
	goose.SetDialect("sqlite3")
	err = goose.UpContext(ctx, w, "backup")
	if err != nil {
		panic(err)
	}

	_, err = w.ExecContext(ctx, "PRAGMA journal_mode=WAL")
	if err != nil {
		panic(err)
	}

	_, err = w.ExecContext(ctx, "PRAGMA synchronous=NORMAL")
	if err != nil {
		panic(err)
	}
	d := db{
		w: w,
	}
	d.configure()
	return backup.New(d), nil
}

type db struct {
	w *sql.DB
}

func (s db) ExecContext(ctx context.Context, sql string, args ...any) (sql.Result, error) {
	ts := time.Now()
	res, err := s.w.ExecContext(ctx, sql, args...)
	logger(ctx, "ExecContext", sql, ts, args, res, err)
	return res, err
}

func (s db) PrepareContext(ctx context.Context, sql string) (*sql.Stmt, error) {
	ts := time.Now()
	stmt, err := s.w.PrepareContext(ctx, sql)
	logger(ctx, "PrepareContext", sql, ts, sql, nil, err)
	return stmt, err
}

func (s db) QueryContext(ctx context.Context, sql string, args ...any) (*sql.Rows, error) {
	ts := time.Now()
	rows, err := s.w.QueryContext(ctx, sql, args...)
	logger(ctx, "QueryContext", sql, ts, args, nil, err)
	return rows, err
}

func (s db) QueryRowContext(ctx context.Context, sql string, args ...any) *sql.Row {
	ts := time.Now()
	row := s.w.QueryRowContext(ctx, sql, args...)
	logger(ctx, "QueryRowContext", sql, ts, args, nil, nil)
	return row
}

func relaceConsecutiveSpaces(s string) string {
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

func logger(ctx context.Context, info string, query string, ts time.Time, args any, res sql.Result, err error) {
	timeSince := time.Since(ts).String()
	query = strings.ReplaceAll(query, "\n", " ")
	query = strings.ReplaceAll(query, "\t", " ")
	query = relaceConsecutiveSpaces(query)
	meta := []any{
		slog.String("query", query),
		slog.Any("args", args),
		slog.String("duration", timeSince),
	}

	if res != nil {
		rows, err := res.RowsAffected()
		if err != nil {
			meta = append(meta, slog.String("rows_error", err.Error()))
			slog.Error("Rows affected failed", meta...)
		}
		meta = append(meta, slog.Int64("rows", rows))
	}
	if err != nil {
		meta = append(meta, slog.Any("error", err))
		e := &sqlite.Error{}
		if !errors.As(err, &e) {
			slog.Error(fmt.Sprintf("%s failed", info), meta...)
			return
		}
	}
	slog.Default().Log(ctx, dbotlog.LevelSQL, fmt.Sprintf("%s executed", info), meta...)
}
