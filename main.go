package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/jpillora/backoff"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// Use the eu-west-1 region as default
	awsDefaultRegion = "eu-west-1"

	// Firehose will create objects with a year/month/day/hour structure.
	// Prefix indicates that we are only interested in the events from
	// firehose and not anything else in the bucket.
	firehosePrefix = "20"

	// dataEventMessageType is the Firehose message type for data
	dataEventMessageType = "DATA_MESSAGE"

	// checkInterval is the delay in minutes to check for new Firehose events
	defaultCheckInterval = 2 * time.Minute

	// default proc window
	defaultProcWindowHour = 5
)

var (
	auditEvents = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "feab_audit_event_total",
			Help: "How many audit logs have been processed.",
		},
	)

	errorEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "feab_errors_total",
			Help: "How many errors encountered.",
		},
		[]string{"type"},
	)
)

// Log is a CloudWatch K8s audit log line
type Log struct {
	Message string `json:"message"`
}

// Event is a firehose event
type Event struct {
	MessageType string `json:"messageType"`
	LogEvents   []Log  `json:"logEvents"`
}

func init() {

	// Track the amount of K8s audit logs processed and errors encountered
	prometheus.MustRegister(auditEvents)
	prometheus.MustRegister(errorEvents)

	// Register the HTTP handlers
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusOK)
	})

	// Start the monitoring service
	go http.ListenAndServe(":8080", nil)
	fmt.Printf("Started monitoring services\n")
}

// validJSON parses the JSON while discarding the contents.
// It returns true for valid JSON, false otherwise.
func validJSON(data string) bool {
	dec := json.NewDecoder(bytes.NewReader([]byte(data)))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return true
		}
		if err != nil {
			return false
		}
	}
}

func checkReadiness(falcoEndpoint string, bucket_name string, region string) error {
	// check if falco is available
	httpClient := retryablehttp.NewClient()
	_, err := httpClient.Get(falcoEndpoint)

	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Error: %s is unreachable", falcoEndpoint))
	}

	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(region),
	}))
	s3client := s3.New(sess)

	// check if s3 bucket is reachable in terms of list operation
	_, err = s3client.ListObjects(&s3.ListObjectsInput{
		Bucket: aws.String(bucket_name),
		Prefix: aws.String("/"),
	})
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Error: fail to list object. bucket name is %s", bucket_name))
	}

	// check if s3 bucket is reachable in terms of put operation
	buf := bytes.NewReader([]byte("hello"))

	uploader := s3manager.NewUploader(sess)
	upParams := &s3manager.UploadInput{
		Bucket: aws.String(bucket_name),
		Key:    aws.String(fmt.Sprintf("%s", "falco-audit-bridge-s3test")),
		Body:   buf,
	}

	_, err = uploader.Upload(upParams)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Error: fail to upload object. bucket name is %s", bucket_name))
	}

	// check if s3 bucket is reachable in terms of delete operation
	_, err = s3client.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(bucket_name),
		Key:    aws.String(fmt.Sprintf("%s", "falco-audit-bridge-s3test")),
	})
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Error: fail to delete object. bucket name is %s", bucket_name))
	}

	return nil
}

func moveLogObject(region string, bucketName string, objName string, tgtPrefix string) error {
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(region),
	}))
	s3client := s3.New(sess)

	_, err := s3client.CopyObject(&s3.CopyObjectInput{
		Bucket:     aws.String(bucketName),
		CopySource: aws.String(fmt.Sprintf("/%s/%s", bucketName, objName)),
		Key:        aws.String(fmt.Sprintf("%s/%s", tgtPrefix, objName)),
	})
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Error: Could not copy file to folder %s:\n%v\n", tgtPrefix, err))
	}

	_, err = s3client.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(objName),
	})
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Error: Could not delete file from the Firehose folder %s:\n%v\n", tgtPrefix, err))
	}

	return nil

}

func main() {

	bucket, ok := os.LookupEnv("BUCKET")
	if !ok {
		fmt.Println("Environment variable 'BUCKET' not set, exiting.")
		os.Exit(1)
	}

	falcoEndpoint, ok := os.LookupEnv("FALCO_ENDPOINT")
	if !ok {
		fmt.Println("Environment variable 'FALCO_ENDPOINT' not set, exiting.")
		os.Exit(1)
	}

	region, ok := os.LookupEnv("AWS_DEFAULT_REGION")
	if !ok {
		region = awsDefaultRegion
	}

	prefix, ok := os.LookupEnv("FIREHOSE_PREFIX")
	if !ok {
		prefix = firehosePrefix
	}

	skip_error_log_tmp, ok := os.LookupEnv("SKIP_ERROR_LOG")
	if !ok {
		skip_error_log_tmp = "false"
	}

	skip_error_log, err := strconv.ParseBool(skip_error_log_tmp)
	if err != nil {
		skip_error_log = false
	}

	checkInterval := defaultCheckInterval
	checkInterval_tmp, ok := os.LookupEnv("CHECK_INTERVAL_SECOND")
	if ok {
		checkInterval_tmp2, err := strconv.Atoi(checkInterval_tmp)
		if err != nil {
			fmt.Errorf("error: fail to parse params, CHECK_INTERVAL_SECOND")
			os.Exit(1)
		}
		checkInterval = time.Duration(checkInterval_tmp2) * time.Second
	}

	procWindowHour := defaultProcWindowHour
	procWindowHour_tmp, ok := os.LookupEnv("PROC_WINDOW_HOUR")
	if ok {
		procWindowHour, err = strconv.Atoi(procWindowHour_tmp)
		if err != nil {
			procWindowHour = defaultProcWindowHour
		}
	}

	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(region),
	}))
	s3client := s3.New(sess)

	b := &backoff.Backoff{
		Jitter: true,
	}

	err = checkReadiness(falcoEndpoint, bucket, region)

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	httpClient := retryablehttp.NewClient()

	// Disable the verbose debug logging for now
	httpClient.Logger = nil

	for {
		res, err := s3client.ListObjects(&s3.ListObjectsInput{
			Bucket: aws.String(bucket),
			Prefix: aws.String(prefix),
		})

		if err != nil {
			d := b.Duration()
			fmt.Printf("Error listing bucket:\n%v\nRetrying in %s", err, d)
			errorEvents.With(prometheus.Labels{"type": "s3-list"}).Inc()
			time.Sleep(d)
			continue
		}
		b.Reset()

		objects := res.Contents

		// Sort the objects according to LastModified date
		sort.Slice(objects, func(i, j int) bool {
			return objects[i].LastModified.Before(*objects[j].LastModified)
		})

		for _, object := range objects {

			// Check if the object was already processed but somehow not deleted
			_, err := s3client.HeadObject(&s3.HeadObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(fmt.Sprintf("processed/%s", *object.Key)),
			})

			if err != nil {
				// Apparently the object doesn't exist in the processed folder, so start processing it
				fmt.Printf("Processing: %s\n", *object.Key)
			} else {
				// The object already exists in the processed folder, so delete and ignore it
				_, err = s3client.DeleteObject(&s3.DeleteObjectInput{
					Bucket: aws.String(bucket),
					Key:    object.Key,
				})

				if err != nil {
					fmt.Printf("Could not delete file from the Firehose folder:\n%v\n", err)
					errorEvents.With(prometheus.Labels{"type": "s3-delete"}).Inc()
					continue
				}
			}

			// Check if the object is not over-aged
			now := time.Now()
			sub := now.Sub(*object.LastModified)

			if sub.Hours() > float64(procWindowHour) {
				fmt.Printf("Skipping: object '%s' is too old to proc. (window size is %d hour)\n", *object.Key, procWindowHour)
				err := moveLogObject(region, bucket, *object.Key, "processed")
				if err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
				continue
			}

			// Download the file from S3
			file, err := s3client.GetObject(
				&s3.GetObjectInput{
					Bucket: aws.String(bucket),
					Key:    object.Key,
				})
			if err != nil {
				fmt.Printf("Could not download the Firehose events file:\n%v\n", err)
				errorEvents.With(prometheus.Labels{"type": "s3-get"}).Inc()
				continue
			}

			// Uncompress the file
			contents, err := gzip.NewReader(file.Body)
			if err != nil {
				fmt.Printf("Could not decompress the Firehose events:\n%v\n", err)
				errorEvents.With(prometheus.Labels{"type": "gzip"}).Inc()

				if skip_error_log {
					err := moveLogObject(region, bucket, *object.Key, "error")
					if err != nil {
						fmt.Println(err)
						os.Exit(1)
					}
				}
				continue
			}

			// Parse the JSON file contents
			decoder := json.NewDecoder(contents)
			event := Event{}

			var processed bool
		DECODER:
			for {

				if err := decoder.Decode(&event); err == io.EOF {
					processed = true
					break
				} else if err != nil {
					fmt.Printf("Unable to (fully) parse Firehose event:\n%v\n", err)
					errorEvents.With(prometheus.Labels{"type": "parsing"}).Inc()
					break
				}

				if event.MessageType == dataEventMessageType {
					for _, log := range event.LogEvents {
						if validJSON(log.Message) {
							// Post the audit log to Falco for compliance checking
							res, err := httpClient.Post(falcoEndpoint, "application/json", strings.NewReader(log.Message))
							if err != nil {
								fmt.Printf("Unable to send the audit log to Falco.\nError: %v\n", err)
								errorEvents.With(prometheus.Labels{"type": "falco"}).Inc()
								break DECODER
							}

							if res.StatusCode != 200 {
								fmt.Printf("Falco did not accept the audit log\nLog: %s, Code: %d\n", log.Message, res.StatusCode)
								errorEvents.With(prometheus.Labels{"type": "falco"}).Inc()
								break DECODER
							}

							res.Body.Close()
						}
					}
				}
			}

			if processed {
				// Track successfull processing
				auditEvents.Inc()

				err := moveLogObject(region, bucket, *object.Key, "processed")
				if err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
			} else {
				fmt.Printf("Object '%s' was not (fully) processed, not moving it to the processed folder.\n", *object.Key)

				if skip_error_log {
					fmt.Printf("Skipping error object '%s' by moving it to error folder.\n", *object.Key)
					err := moveLogObject(region, bucket, *object.Key, "error")
					if err != nil {
						fmt.Println(err)
						os.Exit(1)
					}
				}
			}
		}

		if len(objects) == 0 {
			fmt.Printf("No new Firehose events found, waiting for next check interval. interval = %.1f sec\n", checkInterval.Seconds())
			time.Sleep(checkInterval)
		}
	}
}
