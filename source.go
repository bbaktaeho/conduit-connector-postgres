// Copyright © 2022 Meroxa, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/conduitio/conduit-commons/csync"
	"github.com/conduitio/conduit-connector-postgres/source"
	"github.com/conduitio/conduit-connector-postgres/source/logrepl"
	sdk "github.com/conduitio/conduit-connector-sdk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Source is a Postgres source plugin.
type Source struct {
	sdk.UnimplementedSource

	iterator  source.Iterator
	config    source.Config
	pool      *pgxpool.Pool
	tableKeys map[string]string
}

func NewSource() sdk.Source {
	return sdk.SourceWithMiddleware(
		&Source{
			tableKeys: make(map[string]string),
		},
		sdk.DefaultSourceMiddleware()...,
	)
}

func (s *Source) Parameters() map[string]sdk.Parameter {
	return s.config.Parameters()
}

func (s *Source) Configure(_ context.Context, cfg map[string]string) error {
	err := sdk.Util.ParseConfig(cfg, &s.config)
	if err != nil {
		return err
	}

	s.config = s.config.Init()

	return s.config.Validate()
}

func (s *Source) Open(ctx context.Context, pos sdk.Position) error {
	pool, err := pgxpool.New(ctx, s.config.URL)
	if err != nil {
		return fmt.Errorf("failed to create a connection pool to database: %w", err)
	}
	s.pool = pool

	logger := sdk.Logger(ctx)
	if s.readingAllTables() {
		logger.Info().Msg("Detecting all tables...")
		s.config.Tables, err = s.getAllTables(ctx)
		if err != nil {
			return fmt.Errorf("failed to connect to get all tables: %w", err)
		}
		logger.Info().
			Strs("tables", s.config.Tables).
			Int("count", len(s.config.Tables)).
			Msg("Successfully detected tables")
	}

	// ensure we have keys for all tables
	for _, tableName := range s.config.Tables {
		s.tableKeys[tableName], err = s.getPrimaryKey(ctx, tableName)
		if err != nil {
			return fmt.Errorf("failed to find primary key for table %s: %w", tableName, err)
		}
	}

	switch s.config.CDCMode {
	case source.CDCModeAuto:
		// TODO add logic that checks if the DB supports logical replication (since that's the only thing we support at the moment)
		fallthrough
	case source.CDCModeLogrepl:
		i, err := logrepl.NewCombinedIterator(ctx, s.pool, logrepl.Config{
			Position:        pos,
			SlotName:        s.config.LogreplSlotName,
			PublicationName: s.config.LogreplPublicationName,
			Tables:          s.config.Tables,
			TableKeys:       s.tableKeys,
			WithSnapshot:    s.config.SnapshotMode == source.SnapshotModeInitial,
		})
		if err != nil {
			return fmt.Errorf("failed to create logical replication iterator: %w", err)
		}
		s.iterator = i
	default:
		// shouldn't happen, config was validated
		return fmt.Errorf("unsupported CDC mode %q", s.config.CDCMode)
	}
	return nil
}

func (s *Source) Read(ctx context.Context) (sdk.Record, error) {
	return s.iterator.Next(ctx)
}

func (s *Source) Ack(ctx context.Context, pos sdk.Position) error {
	return s.iterator.Ack(ctx, pos)
}

func (s *Source) Teardown(ctx context.Context) error {
	logger := sdk.Logger(ctx)

	var errs []error
	if s.iterator != nil {
		logger.Debug().Msg("Tearing down iterator...")
		if err := s.iterator.Teardown(ctx); err != nil {
			logger.Warn().Err(err).Msg("Failed to tear down iterator")
			errs = append(errs, fmt.Errorf("failed to tear down iterator: %w", err))
		}
	}
	if s.pool != nil {
		logger.Debug().Msg("Closing connection pool...")
		err := csync.RunTimeout(ctx, s.pool.Close, time.Minute)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to close DB connection pool: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (s *Source) LifecycleOnDeleted(ctx context.Context, _ map[string]string) error {
	switch s.config.CDCMode {
	case source.CDCModeAuto:
		fallthrough // TODO: Adjust as `auto` changes.
	case source.CDCModeLogrepl:
		if !s.config.LogreplAutoCleanup {
			sdk.Logger(ctx).Warn().Msg("Skipping logrepl auto cleanup")
			return nil
		}

		return logrepl.Cleanup(ctx, logrepl.CleanupConfig{
			URL:             s.config.URL,
			SlotName:        s.config.LogreplSlotName,
			PublicationName: s.config.LogreplPublicationName,
		})
	default:
		return nil
	}
}

func (s *Source) readingAllTables() bool {
	return len(s.config.Tables) == 1 && s.config.Tables[0] == source.AllTablesWildcard
}

func (s *Source) getAllTables(ctx context.Context) ([]string, error) {
	query := "SELECT tablename FROM pg_tables WHERE schemaname = 'public'"

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			return nil, fmt.Errorf("failed to scan table name: %w", err)
		}
		tables = append(tables, tableName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return tables, nil
}

// getPrimaryKey queries the db for the name of the primary key column for a
// table if one exists and returns it.
func (s *Source) getPrimaryKey(ctx context.Context, tableName string) (string, error) {
	query := `SELECT c.column_name
FROM information_schema.table_constraints tc
JOIN information_schema.constraint_column_usage AS ccu USING (constraint_schema, constraint_name)
JOIN information_schema.columns AS c ON c.table_schema = tc.constraint_schema
  AND tc.table_name = c.table_name AND ccu.column_name = c.column_name
WHERE constraint_type = 'PRIMARY KEY' AND tc.table_schema = 'public'
  AND tc.table_name = $1`

	rows, err := s.pool.Query(ctx, query, tableName)
	if err != nil {
		return "", fmt.Errorf("failed to query table keys: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if rows.Err() != nil {
			return "", fmt.Errorf("query failed: %w", rows.Err())
		}
		return "", fmt.Errorf("no table keys found: %w", pgx.ErrNoRows)
	}

	var colName string
	err = rows.Scan(&colName)
	if err != nil {
		return "", fmt.Errorf("failed to scan row: %w", err)
	}

	if rows.Next() {
		// we only support single column primary keys for now
		return "", errors.New("composite keys are not supported")
	}

	return colName, nil
}
