package conns3

import (
	"fmt"

	"github.com/PeerDB-io/peer-flow/connectors/utils"
	avro "github.com/PeerDB-io/peer-flow/connectors/utils/avro"
	"github.com/PeerDB-io/peer-flow/generated/protos"
	"github.com/PeerDB-io/peer-flow/model"
	log "github.com/sirupsen/logrus"
)

func (c *S3Connector) GetQRepPartitions(config *protos.QRepConfig,
	last *protos.QRepPartition,
) ([]*protos.QRepPartition, error) {
	panic("not implemented for s3")
}

func (c *S3Connector) PullQRepRecords(config *protos.QRepConfig,
	partition *protos.QRepPartition,
) (*model.QRecordBatch, error) {
	panic("not implemented for s3")
}

func (c *S3Connector) SyncQRepRecords(
	config *protos.QRepConfig,
	partition *protos.QRepPartition,
	stream *model.QRecordStream,
) (int, error) {
	schema, err := stream.Schema()
	if err != nil {
		log.WithFields(log.Fields{
			"flowName":    config.FlowJobName,
			"partitionID": partition.PartitionId,
		}).Errorf("failed to get schema from stream: %v", err)
		return 0, fmt.Errorf("failed to get schema from stream: %w", err)
	}

	dstTableName := config.DestinationTableIdentifier
	avroSchema, err := getAvroSchema(dstTableName, schema)
	if err != nil {
		return 0, err
	}

	numRecords, err := c.writeToAvroFile(stream, avroSchema, partition.PartitionId, config.FlowJobName)
	if err != nil {
		return 0, err
	}

	return numRecords, nil
}

func getAvroSchema(
	dstTableName string,
	schema *model.QRecordSchema,
) (*model.QRecordAvroSchemaDefinition, error) {
	avroSchema, err := model.GetAvroSchemaDefinition(dstTableName, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to define Avro schema: %w", err)
	}

	return avroSchema, nil
}

func (c *S3Connector) writeToAvroFile(
	stream *model.QRecordStream,
	avroSchema *model.QRecordAvroSchemaDefinition,
	partitionID string,
	jobName string,
) (int, error) {
	s3o, err := utils.NewS3BucketAndPrefix(c.url)
	if err != nil {
		return 0, fmt.Errorf("failed to parse bucket path: %w", err)
	}

	s3Key := fmt.Sprintf("%s/%s/%s.avro", s3o.Prefix, jobName, partitionID)
	writer := avro.NewPeerDBOCFWriter(c.ctx, stream, avroSchema)
	numRecords, err := writer.WriteRecordsToS3(s3o.Bucket, s3Key)
	if err != nil {
		return 0, fmt.Errorf("failed to write records to S3: %w", err)
	}

	return numRecords, nil
}

// S3 just sets up destination, not metadata tables
func (c *S3Connector) SetupQRepMetadataTables(config *protos.QRepConfig) error {
	log.Infof("QRep metadata setup not needed for S3.")
	return nil
}

func (c *S3Connector) ConsolidateQRepPartitions(config *protos.QRepConfig) error {
	log.Infof("Consolidate partitions not needed for S3.")
	return nil
}

func (c *S3Connector) CleanupQRepFlow(config *protos.QRepConfig) error {
	log.Infof("Cleanup QRep Flow not needed for S3.")
	return nil
}
