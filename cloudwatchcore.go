package zapcloudwatchcore

import (
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"go.uber.org/zap/zapcore"
)

// CloudwatchCore is a zap Core for dispatching messages to the specified
type CloudwatchCore struct {
	// Messages with a log level not contained in this array
	// will not be dispatched. If nil, all messages will be dispatched.
	AcceptedLevels    []zapcore.Level
	GroupName         string
	StreamName        string
	AWSConfig         *aws.Config
	nextSequenceToken *string
	svc               *cloudwatchlogs.CloudWatchLogs
	Async             bool // if async is true, send a message asynchronously.
	m                 sync.Mutex

	zapcore.LevelEnabler
	enc zapcore.Encoder
	out zapcore.WriteSyncer
}

// https://github.com/uber-go/zap/blob/master/example_test.go
// https://godoc.org/go.uber.org/zap#hdr-Extending_Zap
// https://github.com/uber-go/zap/blob/master/zapcore/core.go

type NewCloudwatchCoreParams struct {
	GroupName    string
	StreamName   string
	IsAsync      bool
	Config       *aws.Config
	Level        zapcore.Level
	Enc          zapcore.Encoder
	Out          zapcore.WriteSyncer
	LevelEnabler zapcore.LevelEnabler
}

func NewCloudwatchCore(params *NewCloudwatchCoreParams) (zapcore.Core, error) {
	core := &CloudwatchCore{
		GroupName:      params.GroupName,
		StreamName:     params.StreamName,
		AWSConfig:      params.Config,
		Async:          params.IsAsync,
		AcceptedLevels: LevelThreshold(params.Level),
		LevelEnabler:   params.LevelEnabler,
		enc:            params.Enc,
		out:            params.Out,
	}

	err := core.cloudWatchInit()
	if err != nil {
		return nil, err
	}

	return core, nil
}

func (c *CloudwatchCore) With(fields []zapcore.Field) zapcore.Core {
	clone := c.clone()
	addFields(clone.enc, fields)
	return clone
}

func (c *CloudwatchCore) clone() *CloudwatchCore {
	return &CloudwatchCore{
		GroupName:      c.GroupName,
		StreamName:     c.StreamName,
		AWSConfig:      c.AWSConfig,
		Async:          c.Async,
		AcceptedLevels: c.AcceptedLevels,
		LevelEnabler:   c.LevelEnabler,
		enc:            c.enc.Clone(),
		out:            c.out,
	}
}

func addFields(enc zapcore.ObjectEncoder, fields []zapcore.Field) {
	for i := range fields {
		fields[i].AddTo(enc)
	}
}

func (c *CloudwatchCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *CloudwatchCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	buf, err := c.enc.EncodeEntry(ent, fields)
	if err != nil {
		return err
	}
	err = c.cloudwatchWriter(ent, buf.String())
	buf.Free()

	if err != nil {
		return err
	}

	if ent.Level > zapcore.ErrorLevel {
		// Since we may be crashing the program, sync the output. Ignore Sync
		// errors, pending a clean solution to issue #370.
		c.Sync()
	}

	return nil
}

func (c *CloudwatchCore) Sync() error {
	return c.out.Sync()
}

func (c *CloudwatchCore) cloudwatchWriter(e zapcore.Entry, msg string) error {
	if !c.isAcceptedLevel(e.Level) {
		return nil
	}

	event := &cloudwatchlogs.InputLogEvent{
		Message:   aws.String(fmt.Sprintf("%s", msg)),
		Timestamp: aws.Int64(int64(time.Nanosecond) * time.Now().UnixNano() / int64(time.Millisecond)),
	}
	params := &cloudwatchlogs.PutLogEventsInput{
		LogEvents:     []*cloudwatchlogs.InputLogEvent{event},
		LogGroupName:  aws.String(c.GroupName),
		LogStreamName: aws.String(c.StreamName),
		SequenceToken: c.nextSequenceToken,
	}

	if c.Async {
		go c.sendEvent(params)
		return nil
	}

	return c.sendEvent(params)
}

// GetHook function returns hook to zap
func (c *CloudwatchCore) cloudWatchInit() error {
	c.svc = cloudwatchlogs.New(session.New(c.AWSConfig))

	lgresp, err := c.svc.DescribeLogGroups(&cloudwatchlogs.DescribeLogGroupsInput{LogGroupNamePrefix: aws.String(c.GroupName), Limit: aws.Int64(1)})
	if err != nil {
		return err
	}

	if len(lgresp.LogGroups) < 1 {
		// we need to create this log group
		_, err := c.svc.CreateLogGroup(&cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String(c.GroupName)})
		if err != nil {
			return err
		}
	}

	resp, err := c.svc.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName:        aws.String(c.GroupName), // Required
		LogStreamNamePrefix: aws.String(c.StreamName),
	})
	if err != nil {
		return err
	}

	// grab the next sequence token
	if len(resp.LogStreams) > 0 {
		c.nextSequenceToken = resp.LogStreams[0].UploadSequenceToken
		return nil
	}

	// create stream if it doesn't exist. the next sequence token will be null
	_, err = c.svc.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(c.GroupName),
		LogStreamName: aws.String(c.StreamName),
	})

	if err != nil {
		return err
	}
	return nil
}

func (c *CloudwatchCore) sendEvent(params *cloudwatchlogs.PutLogEventsInput) error {
	c.m.Lock()
	defer c.m.Unlock()

	resp, err := c.svc.PutLogEvents(params)
	if err != nil {
		return err
	}
	c.nextSequenceToken = resp.NextSequenceToken
	return nil
}

// Levels sets which levels to sent to cloudwatch
func (c *CloudwatchCore) Levels() []zapcore.Level {
	if c.AcceptedLevels == nil {
		return AllLevels
	}
	return c.AcceptedLevels
}

func (c *CloudwatchCore) isAcceptedLevel(level zapcore.Level) bool {
	for _, lv := range c.Levels() {
		if lv == level {
			return true
		}
	}
	return false
}

// AllLevels Supported log levels
var AllLevels = []zapcore.Level{
	zapcore.DebugLevel,
	zapcore.InfoLevel,
	zapcore.WarnLevel,
	zapcore.ErrorLevel,
	zapcore.FatalLevel,
	zapcore.PanicLevel,
}

// LevelThreshold - Returns every logging level above and including the given parameter.
func LevelThreshold(l zapcore.Level) []zapcore.Level {
	for i := range AllLevels {
		if AllLevels[i] == l {
			return AllLevels[i:]
		}
	}
	return []zapcore.Level{}
}
