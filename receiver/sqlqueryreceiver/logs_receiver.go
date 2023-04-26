// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sqlqueryreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/sqlqueryreceiver"

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

type logsReceiver struct {
	config           *Config
	settings         receiver.CreateSettings
	createConnection dbProviderFunc
	createClient     clientProviderFunc
	queryReceivers   []*logsQueryReceiver
	nextConsumer     consumer.Logs

	isStarted                bool
	collectionIntervalTicker *time.Ticker
	shutdownRequested        chan struct{}
}

func newLogsReceiver(
	config *Config,
	settings receiver.CreateSettings,
	sqlOpenerFunc sqlOpenerFunc,
	createClient clientProviderFunc,
	nextConsumer consumer.Logs,
) (*logsReceiver, error) {
	receiver := &logsReceiver{
		config:   config,
		settings: settings,
		createConnection: func() (*sql.DB, error) {
			return sqlOpenerFunc(config.Driver, config.DataSource)
		},
		createClient: createClient,
		nextConsumer: nextConsumer,
	}

	receiver.createQueryReceivers()

	return receiver, nil
}

func (receiver *logsReceiver) createQueryReceivers() {
	for i, query := range receiver.config.Queries {
		if len(query.Logs) == 0 {
			continue
		}
		id := component.NewIDWithName("sqlqueryreceiver", fmt.Sprintf("query-%d: %s", i, query.SQL))
		queryReceiver := newLogsQueryReceiver(
			id,
			query,
			receiver.createConnection,
			receiver.createClient,
			receiver.settings.Logger,
		)
		receiver.queryReceivers = append(receiver.queryReceivers, queryReceiver)
	}
}

func (receiver *logsReceiver) Start(ctx context.Context, host component.Host) error {
	if receiver.isStarted {
		receiver.settings.Logger.Debug("requested start, but already started, ignoring.")
		return nil
	}
	receiver.settings.Logger.Debug("starting...")
	receiver.isStarted = true

	for _, queryReceiver := range receiver.queryReceivers {
		err := queryReceiver.start()
		if err != nil {
			return err
		}
	}
	receiver.startCollecting()
	receiver.settings.Logger.Debug("started.")
	return nil
}

func (receiver *logsReceiver) startCollecting() {
	receiver.collectionIntervalTicker = time.NewTicker(receiver.config.CollectionInterval)

	go func() {
		for {
			select {
			case <-receiver.collectionIntervalTicker.C:
				receiver.collect()
			case <-receiver.shutdownRequested:
				return
			}
		}
	}()
}

func (receiver *logsReceiver) collect() {
	logsChannel := make(chan plog.Logs)
	for _, queryReceiver := range receiver.queryReceivers {
		go func(queryReceiver *logsQueryReceiver) {
			logs, err := queryReceiver.collect(context.Background())
			if err != nil {
				receiver.settings.Logger.Error("Error collecting logs", zap.Error(err), zap.Stringer("scraper", queryReceiver.ID()))
			}
			logsChannel <- logs
		}(queryReceiver)
	}

	allLogs := plog.NewLogs()
	for range receiver.queryReceivers {
		logs := <-logsChannel
		logs.ResourceLogs().MoveAndAppendTo(allLogs.ResourceLogs())
	}
	receiver.nextConsumer.ConsumeLogs(context.Background(), allLogs)
}

func (receiver *logsReceiver) Shutdown(ctx context.Context) error {
	if !receiver.isStarted {
		receiver.settings.Logger.Debug("Requested shutdown, but not started, ignoring.")
		return nil
	}

	receiver.stopCollecting()
	for _, queryReceiver := range receiver.queryReceivers {
		queryReceiver.shutdown(ctx)
	}

	receiver.isStarted = false

	return nil
}

func (receiver *logsReceiver) stopCollecting() {
	receiver.collectionIntervalTicker.Stop()
	close(receiver.shutdownRequested)
}

type logsQueryReceiver struct {
	id           component.ID
	query        Query
	createDb     dbProviderFunc
	createClient clientProviderFunc
	logger       *zap.Logger
	db           *sql.DB
	client       dbClient
}

func newLogsQueryReceiver(
	id component.ID,
	query Query,
	dbProviderFunc dbProviderFunc,
	clientProviderFunc clientProviderFunc,
	logger *zap.Logger,
) *logsQueryReceiver {
	queryReceiver := &logsQueryReceiver{
		id:           id,
		query:        query,
		createDb:     dbProviderFunc,
		createClient: clientProviderFunc,
		logger:       logger,
	}
	return queryReceiver
}

func (queryReceiver *logsQueryReceiver) ID() component.ID {
	return queryReceiver.id
}

func (queryReceiver *logsQueryReceiver) start() error {
	var err error
	queryReceiver.db, err = queryReceiver.createDb()
	if err != nil {
		return fmt.Errorf("failed to open db connection: %w", err)
	}
	queryReceiver.client = queryReceiver.createClient(dbWrapper{queryReceiver.db}, queryReceiver.query.SQL, queryReceiver.logger)

	return nil
}

func (queryReceiver *logsQueryReceiver) collect(ctx context.Context) (plog.Logs, error) {
	logs := plog.NewLogs()

	rows, err := queryReceiver.client.queryRows(ctx)
	if err != nil {
		return logs, fmt.Errorf("error getting rows: %w", err)
	}

	var errs error
	scopeLogs := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	for _, logsConfig := range queryReceiver.query.Logs {
		for i, row := range rows {
			if err = rowToLog(row, logsConfig, scopeLogs.AppendEmpty()); err != nil {
				err = fmt.Errorf("row %d: %w", i, err)
				errs = multierr.Append(errs, err)
			}
		}
	}
	return logs, nil
}

func rowToLog(row stringMap, config LogsCfg, logRecord plog.LogRecord) error {
	logRecord.Body().SetStr(row[config.BodyColumn])
	return nil
}

func (queryReceiver *logsQueryReceiver) shutdown(ctx context.Context) error {
	return nil
}