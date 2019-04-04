// Copyright (c) 2018 Ashley Jeffs
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package reader

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"
	"time"

	"github.com/Jeffail/benthos/lib/log"
	"github.com/Jeffail/benthos/lib/message"
	"github.com/Jeffail/benthos/lib/metrics"
	"github.com/Jeffail/benthos/lib/types"
	sess "github.com/Jeffail/benthos/lib/util/aws/session"
	"github.com/Jeffail/gabs"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/sqs"
)

//------------------------------------------------------------------------------

// S3DownloadManagerConfig is a config struct containing fields for an S3
// download manager.
type S3DownloadManagerConfig struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// AmazonS3Config contains configuration values for the AmazonS3 input type.
type AmazonS3Config struct {
	sess.Config     `json:",inline" yaml:",inline"`
	Bucket          string                  `json:"bucket" yaml:"bucket"`
	Prefix          string                  `json:"prefix" yaml:"prefix"`
	Retries         int                     `json:"retries" yaml:"retries"`
	DownloadManager S3DownloadManagerConfig `json:"download_manager" yaml:"download_manager"`
	DeleteObjects   bool                    `json:"delete_objects" yaml:"delete_objects"`
	SQSURL          string                  `json:"sqs_url" yaml:"sqs_url"`
	SQSBodyPath     string                  `json:"sqs_body_path" yaml:"sqs_body_path"`
	SQSBucketPath   string                  `json:"sqs_bucket_path" yaml:"sqs_bucket_path"`
	SQSEnvelopePath string                  `json:"sqs_envelope_path" yaml:"sqs_envelope_path"`
	SQSMaxMessages  int64                   `json:"sqs_max_messages" yaml:"sqs_max_messages"`
	MaxBatchCount   int                     `json:"max_batch_count" yaml:"max_batch_count"`
	Timeout         string                  `json:"timeout" yaml:"timeout"`
}

// NewAmazonS3Config creates a new AmazonS3Config with default values.
func NewAmazonS3Config() AmazonS3Config {
	return AmazonS3Config{
		Config:  sess.NewConfig(),
		Bucket:  "",
		Prefix:  "",
		Retries: 3,
		DownloadManager: S3DownloadManagerConfig{
			Enabled: true,
		},
		DeleteObjects:   false,
		SQSURL:          "",
		SQSBodyPath:     "Records.s3.object.key",
		SQSBucketPath:   "",
		SQSEnvelopePath: "",
		SQSMaxMessages:  10,
		MaxBatchCount:   1,
		Timeout:         "5s",
	}
}

//------------------------------------------------------------------------------

type objKey struct {
	s3Key     string
	s3Bucket  string
	attempts  int
	sqsHandle *sqs.DeleteMessageBatchRequestEntry
}

// AmazonS3 is a benthos reader.Type implementation that reads messages from an
// Amazon S3 bucket.
type AmazonS3 struct {
	conf AmazonS3Config

	sqsBodyPath   []string
	sqsEnvPath    []string
	sqsBucketPath []string

	readKeys   []objKey
	targetKeys []objKey

	readMethod func() (types.Message, error)

	session    *session.Session
	s3         *s3.S3
	downloader *s3manager.Downloader
	sqs        *sqs.SQS
	timeout    time.Duration

	log   log.Modular
	stats metrics.Type
}

// NewAmazonS3 creates a new Amazon S3 bucket reader.Type.
func NewAmazonS3(
	conf AmazonS3Config,
	log log.Modular,
	stats metrics.Type,
) (*AmazonS3, error) {
	var path []string
	if len(conf.SQSBodyPath) > 0 {
		path = strings.Split(conf.SQSBodyPath, ".")
	}
	var bucketPath []string
	if len(conf.SQSBucketPath) > 0 {
		bucketPath = strings.Split(conf.SQSBucketPath, ".")
	}
	var envPath []string
	if len(conf.SQSEnvelopePath) > 0 {
		envPath = strings.Split(conf.SQSEnvelopePath, ".")
	}
	var timeout time.Duration
	if tout := conf.Timeout; len(tout) > 0 {
		var err error
		if timeout, err = time.ParseDuration(tout); err != nil {
			return nil, fmt.Errorf("failed to parse timeout string: %v", err)
		}
	}
	if conf.MaxBatchCount < 1 {
		return nil, fmt.Errorf("max_batch_count '%v' must be > 0", conf.MaxBatchCount)
	}
	s := &AmazonS3{
		conf:          conf,
		sqsBodyPath:   path,
		sqsEnvPath:    envPath,
		sqsBucketPath: bucketPath,
		log:           log,
		stats:         stats,
		timeout:       timeout,
	}
	if conf.DownloadManager.Enabled {
		s.readMethod = s.readFromMgr
	} else {
		s.readMethod = s.read
	}
	return s, nil
}

// Connect attempts to establish a connection to the target S3 bucket and any
// relevant queues used to traverse the objects (SQS, etc).
func (a *AmazonS3) Connect() error {
	if a.session != nil {
		return nil
	}

	sess, err := a.conf.GetSession()
	if err != nil {
		return err
	}

	sThree := s3.New(sess)
	dler := s3manager.NewDownloader(sess)

	if len(a.conf.SQSURL) == 0 {
		listInput := &s3.ListObjectsInput{
			Bucket: aws.String(a.conf.Bucket),
		}
		if len(a.conf.Prefix) > 0 {
			listInput.Prefix = aws.String(a.conf.Prefix)
		}
		err := sThree.ListObjectsPages(listInput,
			func(page *s3.ListObjectsOutput, isLastPage bool) bool {
				for _, obj := range page.Contents {
					a.targetKeys = append(a.targetKeys, objKey{
						s3Key:    *obj.Key,
						attempts: a.conf.Retries,
					})
				}
				return true
			},
		)
		if err != nil {
			return fmt.Errorf("failed to list objects: %v", err)
		}
	} else {
		a.sqs = sqs.New(sess)
	}

	a.log.Infof("Receiving Amazon S3 objects from bucket: %s\n", a.conf.Bucket)

	a.session = sess
	a.downloader = dler
	a.s3 = sThree
	return nil
}

func digStrsFromSlices(slice []interface{}) []string {
	var strs []string
	for _, v := range slice {
		switch t := v.(type) {
		case []interface{}:
			strs = append(strs, digStrsFromSlices(t)...)
		case string:
			strs = append(strs, t)
		}
	}
	return strs
}

func (a *AmazonS3) readSQSEvents() error {
	var dudMessageHandles []*sqs.DeleteMessageBatchRequestEntry

	output, err := a.sqs.ReceiveMessage(&sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(a.conf.SQSURL),
		MaxNumberOfMessages: aws.Int64(a.conf.SQSMaxMessages),
		WaitTimeSeconds:     aws.Int64(int64(a.timeout.Seconds())),
	})
	if err != nil {
		return err
	}

messageLoop:
	for _, sqsMsg := range output.Messages {
		msgHandle := &sqs.DeleteMessageBatchRequestEntry{
			Id:            sqsMsg.MessageId,
			ReceiptHandle: sqsMsg.ReceiptHandle,
		}

		if sqsMsg.Body == nil {
			dudMessageHandles = append(dudMessageHandles, msgHandle)
			continue messageLoop
		}

		gObj, err := gabs.ParseJSON([]byte(*sqsMsg.Body))
		if err != nil {
			dudMessageHandles = append(dudMessageHandles, msgHandle)
			a.log.Errorf("Failed to parse SQS message body: %v\n", err)
			continue messageLoop
		}

		if len(a.sqsEnvPath) > 0 {
			switch t := gObj.S(a.sqsEnvPath...).Data().(type) {
			case string:
				if gObj, err = gabs.ParseJSON([]byte(t)); err != nil {
					dudMessageHandles = append(dudMessageHandles, msgHandle)
					a.log.Errorf("Failed to parse SQS message envelope: %v\n", err)
					continue messageLoop
				}
			case []interface{}:
				docs := []interface{}{}
				strs := digStrsFromSlices(t)
				for _, v := range strs {
					var gObj2 interface{}
					if err2 := json.Unmarshal([]byte(v), &gObj2); err2 == nil {
						docs = append(docs, gObj2)
					}
				}
				if len(docs) == 0 {
					dudMessageHandles = append(dudMessageHandles, msgHandle)
					a.log.Errorf("Failed to parse SQS message envelope: %v\n", err)
					continue messageLoop
				}
				gObj, _ = gabs.Consume(docs)
			default:
				dudMessageHandles = append(dudMessageHandles, msgHandle)
				a.log.Errorf("Unexpected envelope value: %v", t)
				continue messageLoop
			}
		}

		var buckets []string
		switch t := gObj.S(a.sqsBucketPath...).Data().(type) {
		case string:
			buckets = []string{t}
		case []interface{}:
			buckets = digStrsFromSlices(t)
		}

		switch t := gObj.S(a.sqsBodyPath...).Data().(type) {
		case string:
			if strings.HasPrefix(t, a.conf.Prefix) {
				bucket := ""
				if len(buckets) > 0 {
					bucket = buckets[0]
				}
				a.targetKeys = append(a.targetKeys, objKey{
					s3Key:     t,
					s3Bucket:  bucket,
					attempts:  a.conf.Retries,
					sqsHandle: msgHandle,
				})
			}
		case []interface{}:
			newTargets := []string{}
			strs := digStrsFromSlices(t)
			for _, p := range strs {
				if strings.HasPrefix(p, a.conf.Prefix) {
					newTargets = append(newTargets, p)
				}
			}
			if len(newTargets) == 0 {
				dudMessageHandles = append(dudMessageHandles, msgHandle)
			} else {
				for i, target := range newTargets {
					bucket := ""
					if len(buckets) > i {
						bucket = buckets[i]
					}
					a.targetKeys = append(a.targetKeys, objKey{
						s3Key:    target,
						s3Bucket: bucket,
						attempts: a.conf.Retries,
					})
				}
				a.targetKeys[len(a.targetKeys)-1].sqsHandle = msgHandle
			}
		}
	}

	// Discard any SQS messages not associated with a target file.
	for len(dudMessageHandles) > 0 {
		input := sqs.DeleteMessageBatchInput{
			QueueUrl: aws.String(a.conf.SQSURL),
			Entries:  dudMessageHandles,
		}

		// trim input entries to max size
		if len(dudMessageHandles) > 10 {
			input.Entries, dudMessageHandles = dudMessageHandles[:10], dudMessageHandles[10:]
		} else {
			dudMessageHandles = nil
		}
		a.sqs.DeleteMessageBatch(&input)
	}

	if len(a.targetKeys) == 0 {
		return types.ErrTimeout
	}
	return nil
}

func (a *AmazonS3) popTargetKey() {
	if len(a.targetKeys) == 0 {
		return
	}
	target := a.targetKeys[0]
	if len(a.targetKeys) > 1 {
		a.targetKeys = a.targetKeys[1:]
	} else {
		a.targetKeys = nil
	}
	a.readKeys = append(a.readKeys, target)
}

// Read attempts to read a new message from the target S3 bucket.
func (a *AmazonS3) Read() (types.Message, error) {
	return a.readMethod()
}

func addS3Metadata(p types.Part, obj *s3.GetObjectOutput) {
	meta := p.Metadata()
	if obj.LastModified != nil {
		meta.Set("s3_last_modified", obj.LastModified.Format(time.RFC3339))
		meta.Set("s3_last_modified_unix", strconv.FormatInt(obj.LastModified.Unix(), 10))
	}
	if obj.ContentType != nil {
		meta.Set("s3_content_type", *obj.ContentType)
	}
}

// read attempts to read a new message from the target S3 bucket.
func (a *AmazonS3) read() (types.Message, error) {
	if a.session == nil {
		return nil, types.ErrNotConnected
	}

	timeoutAt := time.Now().Add(a.timeout)

	if len(a.targetKeys) == 0 {
		if a.sqs != nil {
			if err := a.readSQSEvents(); err != nil {
				return nil, err
			}
		} else {
			// If we aren't using SQS but exhausted our targets we are done.
			return nil, types.ErrTypeClosed
		}
	}
	if len(a.targetKeys) == 0 {
		return nil, types.ErrTimeout
	}

	msg := message.New(nil)

	for len(a.targetKeys) > 0 && msg.Len() < a.conf.MaxBatchCount && time.Until(timeoutAt) > 0 {
		target := a.targetKeys[0]

		bucket := a.conf.Bucket
		if len(target.s3Bucket) > 0 {
			bucket = target.s3Bucket
		}
		obj, err := a.s3.GetObject(&s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(target.s3Key),
		})
		if err != nil {
			target.attempts--
			if target.attempts == 0 {
				// Remove the target file from our list.
				a.popTargetKey()
				a.log.Errorf("Failed to download file '%s' from bucket '%s' after '%v' attempts: %v\n", target.s3Key, bucket, a.conf.Retries, err)
			} else {
				a.targetKeys[0] = target
				if msg.Len() == 0 {
					return nil, fmt.Errorf("failed to download file '%s' from bucket '%s': %v", target.s3Key, bucket, err)
				}
				a.log.Errorf("Failed to download file '%s' from bucket '%s': %v\n", target.s3Key, bucket, err)
				break
			}
			continue
		}

		a.popTargetKey()

		bytes, err := ioutil.ReadAll(obj.Body)
		obj.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to download file '%s' from bucket '%s': %v", target.s3Key, bucket, err)
		}

		part := message.NewPart(bytes)
		meta := part.Metadata()
		for k, v := range obj.Metadata {
			meta.Set(k, *v)
		}
		meta.Set("s3_key", target.s3Key)
		meta.Set("s3_bucket", bucket)
		addS3Metadata(part, obj)

		msg.Append(part)
	}

	return msg, nil
}

// readFromMgr attempts to read a new message from the target S3 bucket using a
// download manager.
func (a *AmazonS3) readFromMgr() (types.Message, error) {
	if a.session == nil {
		return nil, types.ErrNotConnected
	}

	timeoutAt := time.Now().Add(a.timeout)

	if len(a.targetKeys) == 0 {
		if a.sqs != nil {
			if err := a.readSQSEvents(); err != nil {
				return nil, err
			}
		} else {
			// If we aren't using SQS but exhausted our targets we are done.
			return nil, types.ErrTypeClosed
		}
	}
	if len(a.targetKeys) == 0 {
		return nil, types.ErrTimeout
	}

	msg := message.New(nil)

	for len(a.targetKeys) > 0 && msg.Len() < a.conf.MaxBatchCount && time.Until(timeoutAt) > 0 {
		target := a.targetKeys[0]

		buff := &aws.WriteAtBuffer{}

		bucket := a.conf.Bucket
		if len(target.s3Bucket) > 0 {
			bucket = target.s3Bucket
		}

		// Write the contents of S3 Object to the file
		if _, err := a.downloader.Download(buff, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(target.s3Key),
		}); err != nil {
			target.attempts--
			if target.attempts == 0 {
				// Remove the target file from our list.
				a.popTargetKey()
				a.log.Errorf("Failed to download file '%s' from bucket '%s' after '%v' attempts: %v\n", target.s3Key, bucket, a.conf.Retries, err)
			} else {
				a.targetKeys[0] = target
				if msg.Len() == 0 {
					return nil, fmt.Errorf("failed to download file '%s' from bucket '%s': %v", target.s3Key, bucket, err)
				}
				a.log.Errorf("Failed to download file '%s' from bucket '%s': %v\n", target.s3Key, bucket, err)
				break
			}
			continue
		}

		a.popTargetKey()

		part := message.NewPart(buff.Bytes())
		part.Metadata().
			Set("s3_key", target.s3Key).
			Set("s3_bucket", bucket)

		msg.Append(part)
	}

	return msg, nil
}

// Acknowledge confirms whether or not our unacknowledged messages have been
// successfully propagated or not.
func (a *AmazonS3) Acknowledge(err error) error {
	if err == nil {
		deleteHandles := []*sqs.DeleteMessageBatchRequestEntry{}
		for _, key := range a.readKeys {
			if a.conf.DeleteObjects {
				bucket := a.conf.Bucket
				if len(key.s3Bucket) > 0 {
					bucket = key.s3Bucket
				}
				if _, serr := a.s3.DeleteObject(&s3.DeleteObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(key.s3Key),
				}); serr != nil {
					a.log.Errorf("Failed to delete consumed object: %v\n", serr)
				}
			}
			if key.sqsHandle != nil {
				deleteHandles = append(deleteHandles, key.sqsHandle)
			}
		}
		for len(deleteHandles) > 0 {
			input := sqs.DeleteMessageBatchInput{
				QueueUrl: aws.String(a.conf.SQSURL),
				Entries:  deleteHandles,
			}

			// trim input entries to max size
			if len(deleteHandles) > 10 {
				input.Entries, deleteHandles = deleteHandles[:10], deleteHandles[10:]
			} else {
				deleteHandles = nil
			}

			if res, serr := a.sqs.DeleteMessageBatch(&input); serr != nil {
				a.log.Errorf("Failed to delete consumed SQS messages: %v\n", serr)
			} else {
				for _, fail := range res.Failed {
					a.log.Errorf("Failed to delete consumed SQS message '%v', response code: %v\n", fail.Id, fail.Code)
				}
			}
		}
		a.readKeys = nil
	} else {
		a.targetKeys = append(a.readKeys, a.targetKeys...)
		a.readKeys = nil
	}
	return nil
}

// CloseAsync begins cleaning up resources used by this reader asynchronously.
func (a *AmazonS3) CloseAsync() {
}

// WaitForClose will block until either the reader is closed or a specified
// timeout occurs.
func (a *AmazonS3) WaitForClose(time.Duration) error {
	return nil
}

//------------------------------------------------------------------------------
