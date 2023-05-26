package cloud_watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	awsSession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
)

var messageId = int64(0)

type CloudWatchJournalRepeater struct {
	conn              *cloudwatchlogs.CloudWatchLogs
	logGroupName      string
	logStreamName     string
	nextSequenceToken string
	logger            *Logger
	config            *Config
}

func NewCloudWatchJournalRepeater(sess *awsSession.Session, logger *Logger, config *Config) (*CloudWatchJournalRepeater, error) {
	conn := cloudwatchlogs.New(sess)
	if logger == nil {
		logger = NewSimpleLogger("repeater", config)
	}

	return &CloudWatchJournalRepeater{
		conn:              conn,
		logGroupName:      config.LogGroupName,
		logStreamName:     config.LogStreamName,
		nextSequenceToken: "",
		logger:            logger,
		config:            config,
	}, nil
}

func (repeater *CloudWatchJournalRepeater) Close() error {
	return nil
}

func (repeater *CloudWatchJournalRepeater) WriteBatch(records []*Record) error {

	debug := repeater.config.Debug
	logger := repeater.logger

	events := make([]*cloudwatchlogs.InputLogEvent, 0, len(records))
	for _, record := range records {

		messageId++
		record.SeqId = messageId

		jsonDataBytes, err := json.MarshalIndent(*record, "", "  ")
		if err != nil {
			return err
		}
		jsonData := string(jsonDataBytes)

		events = append(events, &cloudwatchlogs.InputLogEvent{
			Message:   aws.String(jsonData),
			Timestamp: aws.Int64(int64(record.TimeUsec)),
		})
	}

	putEvents := func() error {
		request := &cloudwatchlogs.PutLogEventsInput{
			LogEvents:     events,
			LogGroupName:  &repeater.logGroupName,
			LogStreamName: &repeater.logStreamName,
		}
		if repeater.nextSequenceToken != "" {
			request.SequenceToken = aws.String(repeater.nextSequenceToken)
		}
		result, err := repeater.conn.PutLogEvents(request)
		if err != nil {
			return err
		}
		repeater.nextSequenceToken = *result.NextSequenceToken

		return nil
	}

	getNextToken := func() error {
		limit := int64(1)
		describeRequest := &cloudwatchlogs.DescribeLogStreamsInput{
			LogGroupName:        &repeater.logGroupName,
			LogStreamNamePrefix: &repeater.logStreamName,
			Limit:               &limit,
		}
		describeOutput, err := repeater.conn.DescribeLogStreams(describeRequest)

		if err != nil {
			return err
		}

		if len(describeOutput.LogStreams) > 0 {
			repeater.nextSequenceToken =
				*describeOutput.LogStreams[0].UploadSequenceToken

			if debug {
				logger.Debug.Println("Next Token ", repeater.nextSequenceToken)
			}

			err = putEvents()
			if err != nil {
				return fmt.Errorf("failed to put events after sequence lookup: : %s %v", err.Error(), err)
			}
			return nil
		}

		return errors.New("failed to put events after looking for next sequence")
	}

	createStream := func() error {

		if debug {
			logger.Debug.Println("Creating log stream ", repeater.logStreamName)
		}

		request := &cloudwatchlogs.CreateLogStreamInput{
			LogGroupName:  &repeater.logGroupName,
			LogStreamName: &repeater.logStreamName,
		}
		_, err := repeater.conn.CreateLogStream(request)
		return err
	}

	createLogGroup := func() error {

		if debug {
			logger.Debug.Println("Creating log group ", repeater.logGroupName)
		}

		request := &cloudwatchlogs.CreateLogGroupInput{
			LogGroupName: &repeater.logGroupName,
		}
		_, err := repeater.conn.CreateLogGroup(request)
		return err
	}

	recoverResourceNotFound := func(awsErr awserr.Error) error {
		// Maybe our log stream doesn't exist yet. We'll try
		// to create it and then, if we're successful, try
		// writing the events again.
		err := createStream()
		if err != nil {
			awsErr, _ = err.(awserr.Error)
			//If you did not create the stream, then maybe you need to create the log group.
			if awsErr.Code() == "ResourceNotFoundException" {
				err = createLogGroup()
				if err != nil {
					return fmt.Errorf("failed to create log group: %s %v", err.Error(), err)
				}
				err = createStream()
				if err != nil {
					return fmt.Errorf("failed to create stream after log group: %s %v", err.Error(), err)
				}

			} else {
				return fmt.Errorf("failed to create stream: %s %v", err.Error(), err)
			}
		}

		err = putEvents()
		if err != nil {
			return fmt.Errorf("failed to put events: %s %v", err.Error(), err)
		}
		return nil

	}

	if repeater.nextSequenceToken == "" {
		getNextToken()
	}

	var originalErr error
	err := putEvents()
	if err != nil {
		originalErr = err
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == "ResourceNotFoundException" {
				err = recoverResourceNotFound(awsErr)
				if err != nil {
					return err
				}
			} else if awsErr.Code() == "DataAlreadyAcceptedException" {
				// This batch was already sent?
				repeater.logger.Error.Printf("DataAlreadyAcceptedException from putEvents : %s %v", err.Error(), err)
				err = getNextToken()
				if err != nil {
					return fmt.Errorf("Next token failed after DataAlreadyAcceptedException :  %s %v", err.Error(), err)
				}
			} else if awsErr.Code() == "InvalidSequenceTokenException" {
				repeater.logger.Error.Printf("InvalidSequenceTokenException from putEvents : %s %v", err.Error(), err)
				err = getNextToken()
				if err != nil {
					return fmt.Errorf("Next token failed after InvalidSequenceTokenException : %s %v", err.Error(), err)
				}
			} else {
				repeater.logger.Error.Printf("Error from putEvents : %s %v", originalErr.Error(), originalErr)
				return fmt.Errorf("failed to put events: : %s %v", originalErr.Error(), originalErr)
			}
		}

	} else {
		if repeater.config.Debug {
			repeater.logger.Info.Println("SENT SUCCESSFULLY")
		}
	}

	return nil
}
