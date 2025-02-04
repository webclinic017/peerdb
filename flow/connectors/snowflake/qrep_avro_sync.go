package connsnowflake

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/PeerDB-io/peer-flow/connectors/utils"
	avro "github.com/PeerDB-io/peer-flow/connectors/utils/avro"
	"github.com/PeerDB-io/peer-flow/connectors/utils/metrics"
	"github.com/PeerDB-io/peer-flow/generated/protos"
	"github.com/PeerDB-io/peer-flow/model"
	util "github.com/PeerDB-io/peer-flow/utils"
	log "github.com/sirupsen/logrus"
	_ "github.com/snowflakedb/gosnowflake"
	"go.temporal.io/sdk/activity"
)

type SnowflakeAvroSyncMethod struct {
	config    *protos.QRepConfig
	connector *SnowflakeConnector
}

func NewSnowflakeAvroSyncMethod(
	config *protos.QRepConfig,
	connector *SnowflakeConnector) *SnowflakeAvroSyncMethod {
	return &SnowflakeAvroSyncMethod{
		config:    config,
		connector: connector,
	}
}

func (s *SnowflakeAvroSyncMethod) SyncRecords(
	dstTableSchema []*sql.ColumnType,
	stream *model.QRecordStream,
	flowJobName string,
) (int, error) {
	dstTableName := s.config.DestinationTableIdentifier

	schema, err := stream.Schema()
	if err != nil {
		return -1, fmt.Errorf("failed to get schema from stream: %w", err)
	}

	log.WithFields(log.Fields{
		"destinationTable": dstTableName,
		"flowName":         flowJobName,
	}).Infof("sync function called and schema acquired")

	avroSchema, err := s.getAvroSchema(dstTableName, schema, flowJobName)
	if err != nil {
		return 0, err
	}

	numRecords, localFilePath, err := s.writeToAvroFile(stream, avroSchema, "17", flowJobName)
	if err != nil {
		return 0, err
	}
	log.WithFields(log.Fields{
		"destinationTable": dstTableName,
		"flowName":         flowJobName,
	}).Infof("written %d records to Avro file", numRecords)

	stage := s.connector.getStageNameForJob(s.config.FlowJobName)
	err = s.connector.createStage(stage, s.config)
	if err != nil {
		return 0, err
	}
	log.WithFields(log.Fields{
		"destinationTable": dstTableName,
		"flowName":         flowJobName,
	}).Infof("Created stage %s", stage)

	allCols, err := s.connector.getColsFromTable(s.config.DestinationTableIdentifier)
	if err != nil {
		return 0, err
	}

	err = s.putFileToStage(localFilePath, stage)
	if err != nil {
		return 0, err
	}
	log.WithFields(log.Fields{
		"destinationTable": dstTableName,
	}).Infof("pushed avro file to stage")

	err = CopyStageToDestination(s.connector, s.config, s.config.DestinationTableIdentifier, stage, allCols)
	if err != nil {
		return 0, err
	}
	log.WithFields(log.Fields{
		"destinationTable": dstTableName,
	}).Infof("copying records into %s from stage %s", s.config.DestinationTableIdentifier, stage)

	return numRecords, nil
}

func (s *SnowflakeAvroSyncMethod) SyncQRepRecords(
	config *protos.QRepConfig,
	partition *protos.QRepPartition,
	dstTableSchema []*sql.ColumnType,
	stream *model.QRecordStream,
) (int, error) {
	startTime := time.Now()
	dstTableName := config.DestinationTableIdentifier

	schema, err := stream.Schema()
	if err != nil {
		return -1, fmt.Errorf("failed to get schema from stream: %w", err)
	}
	log.WithFields(log.Fields{
		"flowName":    config.FlowJobName,
		"partitionID": partition.PartitionId,
	}).Infof("sync function called and schema acquired")

	avroSchema, err := s.getAvroSchema(dstTableName, schema, config.FlowJobName)
	if err != nil {
		return 0, err
	}

	numRecords, localFilePath, err := s.writeToAvroFile(stream, avroSchema, partition.PartitionId, config.FlowJobName)
	if err != nil {
		return 0, err
	}

	if localFilePath != "" {
		defer func() {
			log.Infof("removing temp file %s", localFilePath)
			err := os.Remove(localFilePath)
			if err != nil {
				log.WithFields(log.Fields{
					"flowName":         config.FlowJobName,
					"partitionID":      partition.PartitionId,
					"destinationTable": dstTableName,
				}).Errorf("failed to remove temp file %s: %v", localFilePath, err)
			}
		}()
	}

	stage := s.connector.getStageNameForJob(config.FlowJobName)

	putFileStartTime := time.Now()
	err = s.putFileToStage(localFilePath, stage)
	if err != nil {
		return 0, err
	}
	log.WithFields(log.Fields{
		"flowName":    config.FlowJobName,
		"partitionID": partition.PartitionId,
	}).Infof("Put file to stage in Avro sync for snowflake")
	metrics.LogQRepSyncMetrics(s.connector.ctx, config.FlowJobName, int64(numRecords),
		time.Since(putFileStartTime))

	err = s.insertMetadata(partition, config.FlowJobName, startTime)
	if err != nil {
		return -1, err
	}

	activity.RecordHeartbeat(s.connector.ctx, "finished syncing records")

	return numRecords, nil
}

func (s *SnowflakeAvroSyncMethod) getAvroSchema(
	dstTableName string,
	schema *model.QRecordSchema,
	flowJobName string,
) (*model.QRecordAvroSchemaDefinition, error) {
	avroSchema, err := model.GetAvroSchemaDefinition(dstTableName, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to define Avro schema: %w", err)
	}

	log.WithFields(log.Fields{
		"flowName": flowJobName,
	}).Infof("Avro schema: %v\n", avroSchema)
	return avroSchema, nil
}

func (s *SnowflakeAvroSyncMethod) writeToAvroFile(
	stream *model.QRecordStream,
	avroSchema *model.QRecordAvroSchemaDefinition,
	partitionID string,
	flowJobName string,
) (int, string, error) {
	var numRecords int
	ocfWriter := avro.NewPeerDBOCFWriter(s.connector.ctx, stream, avroSchema)
	if s.config.StagingPath == "" {
		tmpDir, err := os.MkdirTemp("", "peerdb-avro")
		if err != nil {
			return 0, "", fmt.Errorf("failed to create temp dir: %w", err)
		}

		localFilePath := fmt.Sprintf("%s/%s.avro", tmpDir, partitionID)
		log.WithFields(log.Fields{
			"flowName":    flowJobName,
			"partitionID": partitionID,
		}).Infof("writing records to local file %s", localFilePath)
		numRecords, err = ocfWriter.WriteRecordsToAvroFile(localFilePath)
		if err != nil {
			return 0, "", fmt.Errorf("failed to write records to Avro file: %w", err)
		}

		return numRecords, localFilePath, nil
	} else if strings.HasPrefix(s.config.StagingPath, "s3://") {
		s3o, err := utils.NewS3BucketAndPrefix(s.config.StagingPath)
		if err != nil {
			return 0, "", fmt.Errorf("failed to parse staging path: %w", err)
		}

		s3Key := fmt.Sprintf("%s/%s/%s.avro", s3o.Prefix, s.config.FlowJobName, partitionID)
		log.WithFields(log.Fields{
			"flowName":    flowJobName,
			"partitionID": partitionID,
		}).Infof("OCF: Writing records to S3")
		numRecords, err = ocfWriter.WriteRecordsToS3(s3o.Bucket, s3Key)
		if err != nil {
			return 0, "", fmt.Errorf("failed to write records to S3: %w", err)
		}

		return numRecords, "", nil
	}

	return 0, "", fmt.Errorf("unsupported staging path: %s", s.config.StagingPath)
}

func (s *SnowflakeAvroSyncMethod) putFileToStage(localFilePath string, stage string) error {
	if localFilePath == "" {
		log.Infof("no file to put to stage")
		return nil
	}

	activity.RecordHeartbeat(s.connector.ctx, "putting file to stage")
	putCmd := fmt.Sprintf("PUT file://%s @%s", localFilePath, stage)

	sutdown := utils.HeartbeatRoutine(s.connector.ctx, 10*time.Second, func() string {
		return fmt.Sprintf("putting file to stage %s", stage)
	})

	defer func() {
		sutdown <- true
	}()

	if _, err := s.connector.database.Exec(putCmd); err != nil {
		return fmt.Errorf("failed to put file to stage: %w", err)
	}

	log.Infof("put file %s to stage %s", localFilePath, stage)
	return nil
}

func CopyStageToDestination(
	connector *SnowflakeConnector,
	config *protos.QRepConfig,
	dstTableName string,
	stage string,
	allCols []string,
) error {
	log.WithFields(log.Fields{
		"flowName": config.FlowJobName,
	}).Infof("Copying stage to destination %s", dstTableName)
	copyOpts := []string{
		"FILE_FORMAT = (TYPE = AVRO)",
		"MATCH_BY_COLUMN_NAME='CASE_INSENSITIVE'",
		"PURGE = TRUE",
		"ON_ERROR = 'CONTINUE'",
	}

	writeHandler := NewSnowflakeAvroWriteHandler(connector, dstTableName, stage, copyOpts)

	appendMode := true
	if config.WriteMode != nil {
		writeType := config.WriteMode.WriteType
		if writeType == protos.QRepWriteType_QREP_WRITE_MODE_UPSERT {
			appendMode = false
		}
	}

	switch appendMode {
	case true:
		err := writeHandler.HandleAppendMode(config.FlowJobName)
		if err != nil {
			return fmt.Errorf("failed to handle append mode: %w", err)
		}

	case false:
		upsertKeyCols := config.WriteMode.UpsertKeyColumns
		err := writeHandler.HandleUpsertMode(allCols, upsertKeyCols, config.WatermarkColumn,
			config.FlowJobName)
		if err != nil {
			return fmt.Errorf("failed to handle upsert mode: %w", err)
		}
	}

	return nil
}

func (s *SnowflakeAvroSyncMethod) insertMetadata(
	partition *protos.QRepPartition,
	flowJobName string,
	startTime time.Time,
) error {
	insertMetadataStmt, err := s.connector.createMetadataInsertStatement(partition, flowJobName, startTime)
	if err != nil {
		log.WithFields(log.Fields{
			"flowName":    flowJobName,
			"partitionID": partition.PartitionId,
		}).Errorf("failed to create metadata insert statement: %v", err)
		return fmt.Errorf("failed to create metadata insert statement: %v", err)
	}

	if _, err := s.connector.database.Exec(insertMetadataStmt); err != nil {
		log.WithFields(log.Fields{
			"flowName":    flowJobName,
			"partitionID": partition.PartitionId,
		}).Errorf("failed to execute metadata insert statement '%s': %v", insertMetadataStmt, err)
		return fmt.Errorf("failed to execute metadata insert statement: %v", err)
	}

	log.WithFields(log.Fields{
		"flowName":    flowJobName,
		"partitionID": partition.PartitionId,
	}).Infof("inserted metadata for partition %s", partition)
	return nil
}

type SnowflakeAvroWriteHandler struct {
	connector    *SnowflakeConnector
	dstTableName string
	stage        string
	copyOpts     []string
}

// NewSnowflakeAvroWriteHandler creates a new SnowflakeAvroWriteHandler
func NewSnowflakeAvroWriteHandler(
	connector *SnowflakeConnector,
	dstTableName string,
	stage string,
	copyOpts []string,
) *SnowflakeAvroWriteHandler {
	return &SnowflakeAvroWriteHandler{
		connector:    connector,
		dstTableName: dstTableName,
		stage:        stage,
		copyOpts:     copyOpts,
	}
}

func (s *SnowflakeAvroWriteHandler) HandleAppendMode(flowJobName string) error {
	//nolint:gosec
	copyCmd := fmt.Sprintf("COPY INTO %s FROM @%s %s", s.dstTableName, s.stage, strings.Join(s.copyOpts, ","))
	log.Infof("running copy command: %s", copyCmd)
	_, err := s.connector.database.Exec(copyCmd)
	if err != nil {
		return fmt.Errorf("failed to run COPY INTO command: %w", err)
	}

	log.Infof("copied file from stage %s to table %s", s.stage, s.dstTableName)
	return nil
}

func GenerateMergeCommand(
	allCols []string,
	upsertKeyCols []string,
	watermarkCol string,
	tempTableName string,
	dstTable string,
) (string, error) {
	// all cols are acquired from snowflake schema, so let us try to make upsert key cols match the case
	// and also the watermark col, then the quoting should be fine
	caseMatchedCols := map[string]string{}
	for _, col := range allCols {
		caseMatchedCols[strings.ToLower(col)] = col
	}

	for i, col := range upsertKeyCols {
		upsertKeyCols[i] = caseMatchedCols[strings.ToLower(col)]
	}

	watermarkCol, ok := caseMatchedCols[strings.ToLower(watermarkCol)]
	if !ok {
		return "", fmt.Errorf("watermark column '%s' not found in destination table", watermarkCol)
	}

	upsertKeys := []string{}
	partitionKeyCols := []string{}
	for _, key := range upsertKeyCols {
		quotedKey := utils.QuoteIdentifier(key)
		upsertKeys = append(upsertKeys, fmt.Sprintf("dst.%s = src.%s", quotedKey, quotedKey))
		partitionKeyCols = append(partitionKeyCols, quotedKey)
	}
	upsertKeyClause := strings.Join(upsertKeys, " AND ")

	updateSetClauses := []string{}
	insertColumnsClauses := []string{}
	insertValuesClauses := []string{}
	for _, column := range allCols {
		quotedColumn := utils.QuoteIdentifier(column)
		updateSetClauses = append(updateSetClauses, fmt.Sprintf("%s = src.%s", quotedColumn, quotedColumn))
		insertColumnsClauses = append(insertColumnsClauses, quotedColumn)
		insertValuesClauses = append(insertValuesClauses, fmt.Sprintf("src.%s", quotedColumn))
	}
	updateSetClause := strings.Join(updateSetClauses, ", ")
	insertColumnsClause := strings.Join(insertColumnsClauses, ", ")
	insertValuesClause := strings.Join(insertValuesClauses, ", ")

	quotedWMC := utils.QuoteIdentifier(watermarkCol)

	selectCmd := fmt.Sprintf(`
		SELECT *
		FROM %s
		QUALIFY ROW_NUMBER() OVER (PARTITION BY %s ORDER BY %s DESC) = 1
	`, tempTableName, strings.Join(partitionKeyCols, ","), quotedWMC)

	mergeCmd := fmt.Sprintf(`
		MERGE INTO %s dst
		USING (%s) src
		ON %s
		WHEN MATCHED AND src.%s > dst.%s THEN UPDATE SET %s
		WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s)
	`, dstTable, selectCmd, upsertKeyClause, quotedWMC, quotedWMC,
		updateSetClause, insertColumnsClause, insertValuesClause)

	return mergeCmd, nil
}

// HandleUpsertMode handles the upsert mode
func (s *SnowflakeAvroWriteHandler) HandleUpsertMode(
	allCols []string,
	upsertKeyCols []string,
	watermarkCol string,
	flowJobName string,
) error {
	runID, err := util.RandomUInt64()
	if err != nil {
		return fmt.Errorf("failed to generate run ID: %w", err)
	}

	tempTableName := fmt.Sprintf("%s_temp_%d", s.dstTableName, runID)

	//nolint:gosec
	createTempTableCmd := fmt.Sprintf("CREATE TEMPORARY TABLE %s AS SELECT * FROM %s LIMIT 0",
		tempTableName, s.dstTableName)
	if _, err := s.connector.database.Exec(createTempTableCmd); err != nil {
		return fmt.Errorf("failed to create temp table: %w", err)
	}
	log.WithFields(log.Fields{
		"flowName": flowJobName,
	}).Infof("created temp table %s", tempTableName)

	//nolint:gosec
	copyCmd := fmt.Sprintf("COPY INTO %s FROM @%s %s",
		tempTableName, s.stage, strings.Join(s.copyOpts, ","))
	_, err = s.connector.database.Exec(copyCmd)
	if err != nil {
		return fmt.Errorf("failed to run COPY INTO command: %w", err)
	}
	log.Infof("copied file from stage %s to temp table %s", s.stage, tempTableName)

	mergeCmd, err := GenerateMergeCommand(allCols, upsertKeyCols, watermarkCol, tempTableName, s.dstTableName)
	if err != nil {
		return fmt.Errorf("failed to generate merge command: %w", err)
	}

	startTime := time.Now()
	rows, err := s.connector.database.Exec(mergeCmd)
	if err != nil {
		return fmt.Errorf("failed to merge data into destination table '%s': %w", mergeCmd, err)
	}
	rowCount, err := rows.RowsAffected()
	if err == nil {
		totalRowsAtTarget, err := s.connector.getTableCounts([]string{s.dstTableName})
		if err != nil {
			return err
		}
		metrics.LogQRepNormalizeMetrics(s.connector.ctx, flowJobName, rowCount, time.Since(startTime),
			totalRowsAtTarget)
	} else {
		log.WithFields(log.Fields{
			"flowName": flowJobName,
		}).Errorf("failed to get rows affected: %v", err)
	}

	log.WithFields(log.Fields{
		"flowName": flowJobName,
	}).Infof("merged data from temp table %s into destination table %s",
		tempTableName, s.dstTableName)
	return nil
}
